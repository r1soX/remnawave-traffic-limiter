package webhook

import (
	"encoding/json"
	"fmt"
)

type Event struct {
	Scope string         `json:"scope"`
	Event string         `json:"event"`
	Time  string         `json:"timestamp"`
	Data  EventData      `json:"data"`
	Meta  map[string]any `json:"meta"`
}

type EventData struct {
	ID                   int64   `json:"id"`
	ShortUUID            string  `json:"shortUuid"`
	Username             string  `json:"username"`
	Status               string  `json:"status"`
	TrafficLimitBytes    float64 `json:"trafficLimitBytes"`
	TrafficLimitStrategy string  `json:"trafficLimitStrategy"`
	ActiveInternalSquads []struct {
		UUID string `json:"uuid"`
		Name string `json:"name"`
	} `json:"activeInternalSquads"`
}

func (d EventData) Identifier() string {
	if d.ShortUUID != "" {
		return d.ShortUUID
	}
	if d.Username != "" {
		return d.Username
	}
	if d.ID != 0 {
		return fmt.Sprintf("%d", d.ID)
	}
	return ""
}

func ParseEvent(body []byte) (*Event, error) {
	var event Event
	if err := json.Unmarshal(body, &event); err != nil {
		return nil, err
	}
	if event.Event == "" {
		return nil, fmt.Errorf("event is empty")
	}
	return &event, nil
}
