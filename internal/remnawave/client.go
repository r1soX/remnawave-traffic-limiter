package remnawave

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	Client  *http.Client
}

type User struct {
	ID                   int64             `json:"id"`
	ShortUUID            string            `json:"shortUuid"`
	Username             string            `json:"username"`
	Status               string            `json:"status"`
	TelegramID           *int64            `json:"telegramId"`
	Email                *string           `json:"email"`
	TrafficLimitBytes    float64           `json:"trafficLimitBytes"`
	TrafficLimitStrategy string            `json:"trafficLimitStrategy"`
	ExpireAt             string            `json:"expireAt"`
	Description          string            `json:"description"`
	Tag                  string            `json:"tag"`
	HWIDDeviceLimit      int64             `json:"hwidDeviceLimit"`
	ExternalSquadUUID    string            `json:"externalSquadUuid"`
	ActiveInternalSquads InternalSquadList `json:"activeInternalSquads"`
	UserTraffic          UserTraffic       `json:"userTraffic"`
}

// UserCreateOptions contains only fields that are safe to copy from a main
// user when the limiter creates its WhiteList counterpart. Credentials are
// deliberately omitted: Remnawave generates fresh credentials for the pair.
type UserCreateOptions struct {
	Username             string
	Status               string
	TrafficLimitBytes    float64
	TrafficLimitStrategy string
	ExpireAt             string
	Description          string
	Tag                  string
	HWIDDeviceLimit      int64
	ExternalSquadUUID    string
	ActiveInternalSquads []string
}

type UserUpdateOptions struct {
	ID                   int64
	Status               *string
	TrafficLimitBytes    *float64
	TrafficLimitStrategy *string
	ExpireAt             *string
	ActiveInternalSquads []string
}

type UserTraffic struct {
	UsedTrafficBytes         float64 `json:"usedTrafficBytes"`
	LifetimeUsedTrafficBytes float64 `json:"lifetimeUsedTrafficBytes"`
}

type InternalSquad struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type InternalSquadList []InternalSquad

func (l *InternalSquadList) UnmarshalJSON(data []byte) error {
	if string(data) == "null" || len(data) == 0 {
		*l = nil
		return nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err == nil {
		out := make(InternalSquadList, 0, len(values))
		for _, raw := range values {
			var item InternalSquad
			if err := json.Unmarshal(raw, &item); err == nil {
				if item.UUID == "" {
					var s string
					if err2 := json.Unmarshal(raw, &s); err2 == nil {
						item.UUID = s
					}
				}
				out = append(out, item)
			}
		}
		*l = out
		return nil
	}
	var single InternalSquad
	if err := json.Unmarshal(data, &single); err == nil {
		*l = InternalSquadList{single}
		return nil
	}
	return nil
}

func (l InternalSquadList) UUIDs() []string {
	result := make([]string, 0, len(l))
	for _, item := range l {
		if item.UUID != "" {
			result = append(result, item.UUID)
		}
	}
	return result
}

type resolveResponse struct {
	Response User `json:"response"`
}

type userResponse struct {
	Response User `json:"response"`
}

type usersResponse struct {
	Response struct {
		Users []User `json:"users"`
		Total int    `json:"total"`
	} `json:"response"`
}

type squadUsageResponse struct {
	Response struct {
		Days []struct {
			Nodes []struct {
				TotalBytes float64 `json:"totalBytes"`
			} `json:"nodes"`
		} `json:"days"`
	} `json:"response"`
}

func NewClient(panelURL, token string) (*Client, error) {
	if panelURL == "" || token == "" {
		return nil, fmt.Errorf("panelURL and token are required")
	}
	return &Client{
		BaseURL: strings.TrimRight(panelURL, "/"),
		Token:   strings.TrimSpace(token),
		Client:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

func (c *Client) ResolveUser(shortUUID string) (*User, error) {
	if strings.TrimSpace(shortUUID) == "" {
		return nil, fmt.Errorf("shortUUID is required")
	}
	urlPath := fmt.Sprintf("%s/api/users/by-short-uuid/%s", c.BaseURL, url.PathEscape(shortUUID))
	var resp resolveResponse
	if err := c.getJSON(urlPath, &resp); err != nil {
		return nil, err
	}
	if resp.Response.ID == 0 {
		return nil, fmt.Errorf("user not found for shortUuid %s", shortUUID)
	}
	return &resp.Response, nil
}

func (c *Client) GetUserByID(id int64) (*User, error) {
	urlPath := fmt.Sprintf("%s/api/users/%d", c.BaseURL, id)
	var resp userResponse
	if err := c.getJSON(urlPath, &resp); err != nil {
		return nil, err
	}
	if resp.Response.ID == 0 {
		return nil, fmt.Errorf("user %d not found", id)
	}
	return &resp.Response, nil
}

// ListUsers returns all panel users using the panel's offset-based pagination.
func (c *Client) ListUsers() ([]User, error) {
	const pageSize = 1000
	users := make([]User, 0)
	for start := 0; ; start += pageSize {
		var resp usersResponse
		urlPath := fmt.Sprintf("%s/api/users?start=%d&size=%d", c.BaseURL, start, pageSize)
		if err := c.getJSON(urlPath, &resp); err != nil {
			return nil, err
		}
		users = append(users, resp.Response.Users...)
		if len(resp.Response.Users) == 0 || len(users) >= resp.Response.Total {
			return users, nil
		}
	}
}

// InternalSquadUsage returns bytes transferred by one user only through nodes
// exposed by the requested internal squad over the inclusive date range.
func (c *Client) InternalSquadUsage(squadUUID string, userID int64, start, end time.Time) (int64, error) {
	if strings.TrimSpace(squadUUID) == "" {
		return 0, fmt.Errorf("internal squad UUID is required")
	}
	if userID <= 0 {
		return 0, fmt.Errorf("user ID is required")
	}
	if end.Before(start) {
		return 0, fmt.Errorf("usage end is before start")
	}
	urlPath := fmt.Sprintf(
		"%s/api/bandwidth-stats/internal-squads/%s/users/%d/usage?start=%s&end=%s",
		c.BaseURL,
		url.PathEscape(squadUUID),
		userID,
		url.QueryEscape(start.Format("2006-01-02")),
		url.QueryEscape(end.Format("2006-01-02")),
	)
	var resp squadUsageResponse
	if err := c.getJSON(urlPath, &resp); err != nil {
		return 0, err
	}
	var total float64
	for _, day := range resp.Response.Days {
		for _, node := range day.Nodes {
			total += node.TotalBytes
		}
	}
	return int64(total), nil
}

func (c *Client) UpdateSquads(userID int64, activeSquads []string) error {
	return c.UpdateUserSettings(userID, activeSquads, nil)
}

// ResetUserTraffic resets one native Remnawave counter. On an explicit new
// billing period the paired limiter calls it concurrently for Main and its
// WhiteList companion; tariff changes and top-ups never trigger this method.
func (c *Client) ResetUserTraffic(userID int64) error {
	if userID <= 0 {
		return fmt.Errorf("user id is required")
	}
	request, err := http.NewRequest(
		http.MethodPost,
		fmt.Sprintf("%s/api/users/%d/actions/reset-traffic", c.BaseURL, userID),
		nil,
	)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("request POST %s failed: %s", request.URL.String(), response.Status)
	}
	return nil
}

// UpdateUserSettings updates squad membership and, when provided, the panel's
// global traffic limit. A zero limit means unlimited in Remnawave.
func (c *Client) UpdateUserSettings(userID int64, activeSquads []string, trafficLimitBytes *float64) error {
	options := UserUpdateOptions{ID: userID, ActiveInternalSquads: activeSquads, TrafficLimitBytes: trafficLimitBytes}
	if trafficLimitBytes != nil {
		strategy := "NO_RESET"
		options.TrafficLimitStrategy = &strategy
	}
	return c.UpdateUser(options)
}

func (c *Client) UpdateUser(options UserUpdateOptions) error {
	if options.ID <= 0 {
		return fmt.Errorf("user id is required")
	}
	payload := map[string]any{"id": options.ID}
	if options.ActiveInternalSquads != nil {
		payload["activeInternalSquads"] = options.ActiveInternalSquads
	}
	if options.TrafficLimitBytes != nil {
		payload["trafficLimitBytes"] = *options.TrafficLimitBytes
	}
	if options.TrafficLimitStrategy != nil {
		payload["trafficLimitStrategy"] = *options.TrafficLimitStrategy
	}
	if options.ExpireAt != nil {
		payload["expireAt"] = *options.ExpireAt
	}
	if options.Status != nil {
		payload["status"] = *options.Status
	}
	return c.doJSON(http.MethodPatch, c.BaseURL+"/api/users", payload, nil)
}

func (c *Client) CreateUser(options UserCreateOptions) (*User, error) {
	if strings.TrimSpace(options.Username) == "" || strings.TrimSpace(options.ExpireAt) == "" {
		return nil, fmt.Errorf("username and expireAt are required")
	}
	payload := map[string]any{
		"username":             options.Username,
		"status":               options.Status,
		"trafficLimitBytes":    options.TrafficLimitBytes,
		"trafficLimitStrategy": options.TrafficLimitStrategy,
		"expireAt":             options.ExpireAt,
		"description":          options.Description,
		"tag":                  options.Tag,
		"hwidDeviceLimit":      options.HWIDDeviceLimit,
		"activeInternalSquads": options.ActiveInternalSquads,
	}
	if options.ExternalSquadUUID != "" {
		payload["externalSquadUuid"] = options.ExternalSquadUUID
	}
	var response userResponse
	if err := c.doJSON(http.MethodPost, c.BaseURL+"/api/users", payload, &response); err != nil {
		return nil, err
	}
	if response.Response.ID == 0 {
		return nil, fmt.Errorf("create user returned an empty user")
	}
	return &response.Response, nil
}

func (c *Client) doJSON(method, urlPath string, payload any, target any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(method, urlPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("request %s %s failed: %s", method, urlPath, response.Status)
	}
	if target != nil {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) getJSON(urlPath string, target any) error {
	request, err := http.NewRequest(http.MethodGet, urlPath, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.Token)
	response, err := c.Client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("request %s failed: %s", urlPath, response.Status)
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func parseInt64(value any) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		i, err := strconv.ParseInt(v.String(), 10, 64)
		if err == nil {
			return i
		}
	}
	return 0
}
