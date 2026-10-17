package collab

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func fakeMenu(n int) AskButton {
	m := AskButton{CustomID: "pick", Placeholder: "Choose one"}
	for i := 0; i < n; i++ {
		m.Options = append(m.Options, SelectOption{Label: fmt.Sprintf("Choice %d", i), Value: fmt.Sprintf("v%d", i), Description: "d"})
	}
	return m
}

func TestSelectValidation(t *testing.T) {
	long := strings.Repeat("x", 101)
	tooManyRows := []AskButton{fakeMenu(2), fakeMenu(2), fakeMenu(2), fakeMenu(2), fakeMenu(2), fakeMenu(2)}
	for i := range tooManyRows {
		tooManyRows[i].CustomID = fmt.Sprintf("m%d", i)
	}
	cases := map[string]struct {
		buttons []AskButton
		want    string
	}{
		"26 options":    {[]AskButton{fakeMenu(26)}, "needs 1-25 options"},
		"no options":    {[]AskButton{{CustomID: "pick"}}, "needs 1-25 options"},
		"no id":         {[]AskButton{{Options: fakeMenu(1).Options}}, "custom_id"},
		"long label":    {[]AskButton{{CustomID: "p", Options: []SelectOption{{Label: long, Value: "a"}}}}, "option label"},
		"long desc":     {[]AskButton{{CustomID: "p", Options: []SelectOption{{Label: "a", Value: "a", Description: long}}}}, "description"},
		"long value":    {[]AskButton{{CustomID: "p", Options: []SelectOption{{Label: "a", Value: long}}}}, "option value"},
		"long hint":     {[]AskButton{{CustomID: "p", Placeholder: long, Options: fakeMenu(1).Options}}, "placeholder"},
		"dup value":     {[]AskButton{{CustomID: "p", Options: []SelectOption{{Label: "a", Value: "a"}, {Label: "b", Value: "a"}}}}, "duplicate option value"},
		"two defaults":  {[]AskButton{{CustomID: "p", Options: []SelectOption{{Label: "a", Value: "a", Default: true}, {Label: "b", Value: "b", Default: true}}}}, "at most one"},
		"too many rows": {tooManyRows, "action rows"},
	}
	for name, c := range cases {
		err := (AskOptions{Buttons: c.buttons}).validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	if err := (AskOptions{Buttons: []AskButton{fakeMenu(25), {Label: "Go"}}}).validate(); err != nil {
		t.Fatalf("valid menu rejected: %v", err)
	}
}

func TestSelectRowsShareMessageWithButtons(t *testing.T) {
	menu := fakeMenu(2)
	menu.Options[1].Emoji = "⭐"
	menu.Options[1].Default = true
	rows := buttonRows([]AskButton{{Label: "A"}, menu, {Label: "B"}}, nil)
	if len(rows) != 3 {
		t.Fatalf("rows = %#v", rows)
	}
	sel := rows[1]["components"].([]map[string]any)
	if len(sel) != 1 || sel[0]["type"] != 3 || sel[0]["custom_id"] != "ask:pick" || sel[0]["placeholder"] != "Choose one" {
		t.Fatalf("select = %#v", sel)
	}
	opts := sel[0]["options"].([]map[string]any)
	if opts[1]["default"] != true || opts[1]["emoji"].(map[string]any)["name"] != "⭐" || opts[0]["description"] != "d" {
		t.Fatalf("options = %#v", opts)
	}
}

func selectInteraction(t *testing.T, user string) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"id": "i1", "token": "tok", "type": 3, "channel_id": "900",
		"channel": map[string]any{"parent_id": "forum"},
		"member":  map[string]any{"user": map[string]any{"id": user}},
		"data":    map[string]any{"custom_id": "ask:pick", "values": []string{"v1"}},
		"message": map[string]any{"id": "500", "components": []any{map[string]any{"type": 1, "components": []any{
			map[string]any{"type": 3, "custom_id": "ask:pick", "options": []any{
				map[string]any{"label": "Choice 0", "value": "v0"},
				map[string]any{"label": "Choice 1", "value": "v1"},
			}},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestSelectPickRecordedLikeTapAfterAck(t *testing.T) {
	a := &answersServer{}
	s := a.server(t)
	defer s.Close()
	l := newListener(t, s)
	if err := l.HandleInteraction(context.Background(), selectInteraction(t, "777")); err != nil {
		t.Fatal(err)
	}
	clicks, _ := l.Store.load()
	if len(clicks) != 1 || clicks[0].Label != "v1" || clicks[0].MessageID != "500" {
		t.Fatalf("clicks = %#v", clicks)
	}
	if len(a.callbacks) != 1 || a.callbacks[0]["type"] != float64(7) {
		t.Fatalf("callbacks = %#v", a.callbacks)
	}
	menu := a.callbacks[0]["data"].(map[string]any)["components"].([]any)[0].(map[string]any)["components"].([]any)[0].(map[string]any)
	opts := menu["options"].([]any)
	if menu["disabled"] != true || opts[1].(map[string]any)["default"] != true || opts[0].(map[string]any)["default"] != nil {
		t.Fatalf("menu = %#v", menu)
	}
}

func TestEditChosenSettlesSelect(t *testing.T) {
	rows := []map[string]any{{"components": []any{
		map[string]any{"type": float64(3), "custom_id": "ask:pick", "options": []any{
			map[string]any{"label": "Choice 0", "value": "v0", "default": true},
			map[string]any{"label": "Choice 1", "value": "v1"},
		}},
	}}}
	got, err := settleFetchedComponents(rows, "v1")
	if err != nil {
		t.Fatal(err)
	}
	menu := got[0]["components"].([]any)[0].(map[string]any)
	opts := menu["options"].([]any)
	if menu["disabled"] != true || opts[1].(map[string]any)["default"] != true || opts[0].(map[string]any)["default"] != nil {
		t.Fatalf("menu = %#v", menu)
	}
	if _, err := settleFetchedComponents(rows, "nope"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
}
