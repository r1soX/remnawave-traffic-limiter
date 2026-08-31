package engine

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"remnawave-traffic-limiter/internal/remnawave"
	"remnawave-traffic-limiter/internal/state"
)

type User struct {
	ID                                                                                               int64
	ShortUUID, Username, Status, TrafficLimitStrategy, ExpireAt, Description, Tag, ExternalSquadUUID string
	TrafficLimitBytes, UsedTrafficBytes                                                              float64
	HWIDDeviceLimit                                                                                  int64
	ActiveInternalSquads                                                                             []string
}

type Processor struct {
	Client                                                   *remnawave.Client
	BasicSquadUUID, WhitelistSquadUUID, LimitNoticeSquadUUID string
	store                                                    *state.Store
	pairedMode                                               bool
}

type ReconcileResult struct {
	State                                                 string
	MainUserID, WhiteUserID, WhiteListUsage, TrafficLimit int64
	Paired                                                bool
}

// PairingIntent is the billing state supplied by Bedolaga immediately after a
// tariff change. It prevents an already-unlimited Main account from masking a
// new WhiteList quota during a retained-pair reactivation.
type PairingIntent struct {
	Enabled              bool
	TrafficLimitBytes    *int64
	TrafficLimitStrategy *string
	ExpireAt             *string
	Status               *string
	ActiveInternalSquads *[]string
	ResetWhiteTraffic    bool
}

func NewProcessor(panelURL, token, basicSquad, whitelistSquad string, _ ...string) (*Processor, error) {
	if basicSquad == "" || whitelistSquad == "" {
		return nil, fmt.Errorf("basic and whitelist squad UUIDs are required")
	}
	client, err := remnawave.NewClient(panelURL, token)
	if err != nil {
		return nil, err
	}
	return &Processor{Client: client, BasicSquadUUID: basicSquad, WhitelistSquadUUID: whitelistSquad}, nil
}

func (p *Processor) ConfigurePairedWhiteList(store *state.Store, notice string) error {
	if p == nil || store == nil {
		return fmt.Errorf("state store is required")
	}
	if strings.TrimSpace(notice) == "" {
		return fmt.Errorf("limit notice squad UUID is required")
	}
	p.store, p.LimitNoticeSquadUUID, p.pairedMode = store, notice, true
	return nil
}

func (p *Processor) PairedWhiteListEnabled() bool {
	return p != nil && p.pairedMode && p.store != nil && p.LimitNoticeSquadUUID != ""
}

func (p *Processor) ResolveUser(identifier string) (*User, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return nil, fmt.Errorf("identifier is required")
	}
	if id, err := strconv.ParseInt(identifier, 10, 64); err == nil {
		u, err := p.Client.GetUserByID(id)
		if err != nil {
			return nil, err
		}
		return userFromAPI(u), nil
	}
	u, err := p.Client.ResolveUser(identifier)
	if err != nil {
		return nil, err
	}
	return userFromAPI(u), nil
}

func (p *Processor) ListUsers() ([]*User, error) {
	apiUsers, err := p.Client.ListUsers()
	if err != nil {
		return nil, err
	}
	users := make([]*User, 0, len(apiUsers))
	for i := range apiUsers {
		users = append(users, userFromAPI(&apiUsers[i]))
	}
	return users, nil
}

func userFromAPI(u *remnawave.User) *User {
	return &User{ID: u.ID, ShortUUID: u.ShortUUID, Username: u.Username, Status: u.Status, TrafficLimitBytes: u.TrafficLimitBytes, TrafficLimitStrategy: u.TrafficLimitStrategy, ExpireAt: u.ExpireAt, Description: u.Description, Tag: u.Tag, HWIDDeviceLimit: u.HWIDDeviceLimit, ExternalSquadUUID: u.ExternalSquadUUID, UsedTrafficBytes: u.UserTraffic.UsedTrafficBytes, ActiveInternalSquads: u.ActiveInternalSquads.UUIDs()}
}

// ReconcilePairedWhiteList is Remnawave-only. A finite user having WhiteList
// receives a second account whose native counter contains only WhiteList flow.
func (p *Processor) ReconcilePairedWhiteList(main *User) (*ReconcileResult, error) {
	if !p.PairedWhiteListEnabled() {
		return nil, fmt.Errorf("paired WhiteList mode is not configured")
	}
	if main == nil || main.ID <= 0 {
		return nil, fmt.Errorf("main user id is required")
	}
	if pair, err := p.store.GetPairByWhiteUserID(main.ID); err == nil {
		if !pair.Enabled {
			return resultFromPair(pair), nil
		}
		// A webhook is often emitted for the technical WhiteList account when
		// its native quota is exhausted.  Returning cached state here delayed
		// LimitNotice until the next polling cycle.  Reload the real Main user
		// and run the normal pair synchronisation immediately instead.
		mainAPI, getErr := p.Client.GetUserByID(pair.MainUserID)
		if getErr != nil {
			return nil, fmt.Errorf("get paired Main user: %w", getErr)
		}
		return p.syncPair(userFromAPI(mainAPI), pair)
	} else if err != sql.ErrNoRows {
		return nil, err
	}
	pair, err := p.store.GetPairByMainUserID(main.ID)
	if err == nil {
		if !pair.Enabled {
			return resultFromPair(pair), nil
		}
		return p.syncPair(main, pair)
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	// The background reconciler only sees the Remnawave user.  Pairing is valid
	// only for a finite tariff that currently exposes both Main and WhiteList.
	// A WhiteList-only tariff is one native user and must never get a companion
	// during a mass rollout.
	if !isEligiblePairCandidate(main, p.BasicSquadUUID, p.WhitelistSquadUUID) {
		return &ReconcileResult{State: state.StateActive, MainUserID: main.ID}, nil
	}
	return p.createPair(main)
}

// isEligiblePairCandidate prevents a bulk reconciliation from creating
// technical accounts for historical, expired panel users. Existing pairs are
// handled before this check so their normal lifecycle keeps working.
func isEligiblePairCandidate(main *User, basicSquadUUID, whitelistSquadUUID string) bool {
	if main == nil || !strings.EqualFold(strings.TrimSpace(main.Status), "ACTIVE") || main.TrafficLimitBytes <= 0 {
		return false
	}
	if !contains(main.ActiveInternalSquads, basicSquadUUID) || !contains(main.ActiveInternalSquads, whitelistSquadUUID) {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, strings.TrimSpace(main.ExpireAt))
	return err == nil && expiresAt.After(time.Now().UTC())
}

func (p *Processor) createPair(main *User) (*ReconcileResult, error) {
	white, err := p.Client.CreateUser(remnawave.UserCreateOptions{Username: whiteUsername(main.Username, main.ID), Status: activeStatus(main.Status), TrafficLimitBytes: main.TrafficLimitBytes, TrafficLimitStrategy: main.TrafficLimitStrategy, ExpireAt: main.ExpireAt, Description: "Managed WhiteList companion for user " + strconv.FormatInt(main.ID, 10), Tag: "WL_LIMITER", HWIDDeviceLimit: main.HWIDDeviceLimit, ExternalSquadUUID: main.ExternalSquadUUID, ActiveInternalSquads: []string{p.WhitelistSquadUUID}})
	if err != nil {
		return nil, fmt.Errorf("create WhiteList user: %w", err)
	}
	zero := float64(0)
	mainStatus := activeStatus(main.Status)
	if err := p.Client.UpdateUser(remnawave.UserUpdateOptions{ID: main.ID, Status: &mainStatus, TrafficLimitBytes: &zero, TrafficLimitStrategy: stringPtr(main.TrafficLimitStrategy), ExpireAt: stringPtr(main.ExpireAt), ActiveInternalSquads: mainOnlySquads(main.ActiveInternalSquads, p.WhitelistSquadUUID, p.LimitNoticeSquadUUID, p.BasicSquadUUID)}); err != nil {
		return nil, fmt.Errorf("make Main user unlimited: %w", err)
	}
	pair := state.PairedUser{MainUserID: main.ID, MainShortUUID: main.ShortUUID, WhiteUserID: white.ID, WhiteShortUUID: white.ShortUUID, QuotaBytes: int64(main.TrafficLimitBytes), TrafficStrategy: main.TrafficLimitStrategy, ExpireAt: main.ExpireAt, LastSourceLimit: int64(main.TrafficLimitBytes), LastSourceExpireAt: main.ExpireAt, State: state.StateActive, Enabled: true}
	if err := p.store.UpsertPair(pair); err != nil {
		return nil, err
	}
	return resultFromPair(&pair), nil
}

func (p *Processor) syncPair(main *User, pair *state.PairedUser) (*ReconcileResult, error) {
	whiteAPI, err := p.Client.GetUserByID(pair.WhiteUserID)
	if err != nil {
		return nil, fmt.Errorf("get WhiteList user: %w", err)
	}
	white := userFromAPI(whiteAPI)
	// Unmodified billing software writes quota/expiry to the original user. Move
	// those values to its companion before Main traffic can consume the quota.
	if main.TrafficLimitBytes > 0 {
		pair.QuotaBytes, pair.TrafficStrategy, pair.LastSourceLimit = int64(main.TrafficLimitBytes), main.TrafficLimitStrategy, int64(main.TrafficLimitBytes)
	}
	if main.ExpireAt != "" {
		pair.ExpireAt, pair.LastSourceExpireAt = main.ExpireAt, main.ExpireAt
	}
	zero := float64(0)
	mainStatus := activeStatus(main.Status)
	if err := p.Client.UpdateUser(remnawave.UserUpdateOptions{ID: main.ID, Status: &mainStatus, TrafficLimitBytes: &zero, TrafficLimitStrategy: stringPtr(pair.TrafficStrategy), ExpireAt: stringPtr(pair.ExpireAt), ActiveInternalSquads: mainOnlySquads(main.ActiveInternalSquads, p.WhitelistSquadUUID, p.LimitNoticeSquadUUID, p.BasicSquadUUID)}); err != nil {
		return nil, fmt.Errorf("sync Main user: %w", err)
	}
	exhausted := pair.QuotaBytes > 0 && int64(white.UsedTrafficBytes) >= pair.QuotaBytes || strings.EqualFold(white.Status, "LIMITED")
	whiteSquads, desiredState := []string{p.WhitelistSquadUUID}, state.StateActive
	if exhausted {
		whiteSquads, desiredState = []string{p.LimitNoticeSquadUUID}, state.StateBlocked
	}
	quota := float64(pair.QuotaBytes)
	whiteStatus := activeStatus(main.Status)
	if err := p.Client.UpdateUser(remnawave.UserUpdateOptions{ID: white.ID, Status: &whiteStatus, TrafficLimitBytes: &quota, TrafficLimitStrategy: stringPtr(pair.TrafficStrategy), ExpireAt: stringPtr(pair.ExpireAt), ActiveInternalSquads: whiteSquads}); err != nil {
		return nil, fmt.Errorf("sync WhiteList user: %w", err)
	}
	pair.State = desiredState
	if err := p.store.UpsertPair(*pair); err != nil {
		return nil, err
	}
	result := resultFromPair(pair)
	result.WhiteListUsage = int64(white.UsedTrafficBytes)
	return result, nil
}

// ApplyPairedWhiteListIntent applies Bedolaga's complete target subscription
// state. A disabled pair is retained in SQLite and in Remnawave, but its
// companion is disabled and absent from the gateway. A later paired target
// reuses that same companion rather than creating another technical account.
func (p *Processor) ApplyPairedWhiteListIntent(main *User, intent PairingIntent) (*ReconcileResult, error) {
	if !p.PairedWhiteListEnabled() {
		return nil, fmt.Errorf("paired WhiteList mode is not configured")
	}
	if main == nil || main.ID <= 0 {
		return nil, fmt.Errorf("main user id is required")
	}
	if intent.TrafficLimitBytes == nil || intent.TrafficLimitStrategy == nil || intent.ExpireAt == nil || intent.Status == nil || intent.ActiveInternalSquads == nil {
		return nil, fmt.Errorf("full tariff target is required")
	}
	if *intent.TrafficLimitBytes < 0 || strings.TrimSpace(*intent.TrafficLimitStrategy) == "" || strings.TrimSpace(*intent.ExpireAt) == "" || strings.TrimSpace(*intent.Status) == "" {
		return nil, fmt.Errorf("invalid full tariff target")
	}
	effective := *main
	if intent.TrafficLimitBytes != nil && *intent.TrafficLimitBytes >= 0 {
		effective.TrafficLimitBytes = float64(*intent.TrafficLimitBytes)
	}
	if intent.TrafficLimitStrategy != nil && strings.TrimSpace(*intent.TrafficLimitStrategy) != "" {
		effective.TrafficLimitStrategy = *intent.TrafficLimitStrategy
	}
	if intent.ExpireAt != nil && strings.TrimSpace(*intent.ExpireAt) != "" {
		effective.ExpireAt = *intent.ExpireAt
	}
	if intent.Status != nil {
		effective.Status = *intent.Status
	}
	if intent.ActiveInternalSquads != nil {
		effective.ActiveInternalSquads = append([]string(nil), (*intent.ActiveInternalSquads)...)
	}

	pair, err := p.store.GetPairByMainUserID(main.ID)
	if err == sql.ErrNoRows {
		if !intent.Enabled {
			if err := p.applySingleTarget(&effective); err != nil {
				return nil, err
			}
			return &ReconcileResult{State: state.StateActive, MainUserID: main.ID}, nil
		}
		if !contains(effective.ActiveInternalSquads, p.WhitelistSquadUUID) {
			effective.ActiveInternalSquads = append(effective.ActiveInternalSquads, p.WhitelistSquadUUID)
		}
		return p.ReconcilePairedWhiteList(&effective)
	}
	if err != nil {
		return nil, err
	}
	if !intent.Enabled {
		// Restore the original account first.  The technical account stays in
		// the database for reuse, but cannot be reached from the gateway after
		// this operation completes.
		if err := p.applySingleTarget(&effective); err != nil {
			return nil, err
		}
		disabled := "DISABLED"
		zero := float64(0)
		if err := p.Client.UpdateUser(remnawave.UserUpdateOptions{
			ID:                   pair.WhiteUserID,
			Status:               &disabled,
			TrafficLimitBytes:    &zero,
			ActiveInternalSquads: []string{},
		}); err != nil {
			return nil, fmt.Errorf("disable WhiteList companion: %w", err)
		}
		pair.Enabled = false
		pair.State = state.StateActive
		if err := p.store.UpsertPair(*pair); err != nil {
			return nil, err
		}
		return resultFromPair(pair), nil
	}

	// Bedolaga has already written the new tariff's desired fields to Main.
	// Keep an explicit copy too: Main can be deliberately unlimited while a
	// paired subscription is active, so it cannot otherwise convey new quota.
	if intent.TrafficLimitBytes != nil && *intent.TrafficLimitBytes >= 0 {
		pair.QuotaBytes = *intent.TrafficLimitBytes
		pair.LastSourceLimit = *intent.TrafficLimitBytes
	}
	if intent.TrafficLimitStrategy != nil && strings.TrimSpace(*intent.TrafficLimitStrategy) != "" {
		pair.TrafficStrategy = *intent.TrafficLimitStrategy
	}
	if intent.ExpireAt != nil && strings.TrimSpace(*intent.ExpireAt) != "" {
		pair.ExpireAt = *intent.ExpireAt
		pair.LastSourceExpireAt = *intent.ExpireAt
	}
	pair.Enabled = true
	if err := p.store.UpsertPair(*pair); err != nil {
		return nil, err
	}
	if intent.ResetWhiteTraffic {
		if err := p.Client.ResetUserTraffic(pair.WhiteUserID); err != nil {
			return nil, fmt.Errorf("reset WhiteList companion traffic: %w", err)
		}
	}
	return p.syncPair(&effective, pair)
}

// applySingleTarget returns control to the original account.  The squads are
// authoritative: this covers both Main-only and WhiteList-only tariffs.  Only
// a tariff containing both access groups uses the technical companion.
// The body comes from Bedolaga's subscription record, so a 150 GiB -> 50 GiB
// tariff switch is a real 50 GiB panel update instead of retaining an old
// companion quota.
func (p *Processor) applySingleTarget(main *User) error {
	if main == nil {
		return fmt.Errorf("main user is required")
	}
	limit := main.TrafficLimitBytes
	mainStatus := activeStatus(main.Status)
	if err := p.Client.UpdateUser(remnawave.UserUpdateOptions{
		ID:                   main.ID,
		Status:               &mainStatus,
		TrafficLimitBytes:    &limit,
		TrafficLimitStrategy: stringPtr(main.TrafficLimitStrategy),
		ExpireAt:             stringPtr(main.ExpireAt),
		ActiveInternalSquads: removeSquads(main.ActiveInternalSquads),
	}); err != nil {
		return fmt.Errorf("restore Main user target: %w", err)
	}
	return nil
}

func resultFromPair(pair *state.PairedUser) *ReconcileResult {
	return &ReconcileResult{State: pair.State, MainUserID: pair.MainUserID, WhiteUserID: pair.WhiteUserID, TrafficLimit: pair.QuotaBytes, Paired: pair.Enabled}
}

// Legacy mode is kept available until the paired migration is enabled.
func (p *Processor) ApplyEvent(event string, user *User) error {
	if event == "user.limited" {
		return p.HandleUserLimited(user)
	}
	if event == "user.traffic_reset" {
		return p.HandleTrafficReset(user)
	}
	return nil
}
func (p *Processor) HandleUserLimited(user *User) error {
	if user == nil || user.ID == 0 || !contains(user.ActiveInternalSquads, p.WhitelistSquadUUID) {
		return nil
	}
	return p.Client.UpdateUser(remnawave.UserUpdateOptions{ID: user.ID, ActiveInternalSquads: removeSquads(user.ActiveInternalSquads, p.WhitelistSquadUUID)})
}
func (p *Processor) HandleTrafficReset(user *User) error {
	if user == nil || user.ID == 0 || contains(user.ActiveInternalSquads, p.WhitelistSquadUUID) {
		return nil
	}
	return p.Client.UpdateUser(remnawave.UserUpdateOptions{ID: user.ID, ActiveInternalSquads: append(removeSquads(user.ActiveInternalSquads), p.WhitelistSquadUUID)})
}
func (p *Processor) HandleUserModified(*User) error { return nil }

func mainOnlySquads(squads []string, white, notice, basic string) []string {
	out := removeSquads(squads, white, notice)
	if !contains(out, basic) {
		out = append(out, basic)
	}
	return out
}
func removeSquads(squads []string, values ...string) []string {
	out := make([]string, 0, len(squads))
	for _, squad := range squads {
		if strings.TrimSpace(squad) == "" {
			continue
		}
		remove := false
		for _, value := range values {
			if strings.EqualFold(strings.TrimSpace(squad), strings.TrimSpace(value)) {
				remove = true
				break
			}
		}
		if !remove && !contains(out, squad) {
			out = append(out, squad)
		}
	}
	return out
}
func contains(values []string, target string) bool {
	for _, v := range values {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(target)) {
			return true
		}
	}
	return false
}
func whiteUsername(username string, id int64) string {
	s := "wl_" + username + "_" + strconv.FormatInt(id, 10)
	if len(s) > 36 {
		return s[:36]
	}
	return s
}
func activeStatus(status string) string {
	if strings.EqualFold(status, "ACTIVE") {
		return "ACTIVE"
	}
	return status
}
func stringPtr(s string) *string { return &s }
