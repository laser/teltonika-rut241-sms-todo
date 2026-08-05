package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T, statePath string) *Config {
	t.Helper()
	config := &Config{
		Router: RouterConfig{
			URL:         "https://192.0.2.1",
			Username:    "admin",
			PasswordEnv: "RUT241_PASSWORD",
			ModemID:     "1-1",
		},
		StateFile:           statePath,
		PollIntervalSeconds: 5,
		MaxTodoLength:       300,
		Users: []UserConfig{
			{
				PhoneNumber:  "+12065550101",
				DailyTime:    "08:00",
				TimezoneName: "America/Los_Angeles",
			},
			{
				PhoneNumber:  "+12065550102",
				DailyTime:    "09:30",
				TimezoneName: "America/New_York",
			},
		},
	}
	if err := config.validate(); err != nil {
		t.Fatalf("validate test config: %v", err)
	}
	return config
}

func testService(t *testing.T) (*Config, *StateStore, *TodoService) {
	t.Helper()
	config := testConfig(t, filepath.Join(t.TempDir(), "state.json"))
	store, err := loadState(config.StateFile, config)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return config, store, newTodoService(config, store)
}

type fakeRouter struct {
	messages []Message
	sent     []sentMessage
}

type sentMessage struct {
	number  string
	message string
}

func (r *fakeRouter) GetMessages(context.Context) ([]Message, error) {
	return append([]Message(nil), r.messages...), nil
}

func (r *fakeRouter) SendSMS(_ context.Context, number string, message string) error {
	r.sent = append(r.sent, sentMessage{number: number, message: message})
	return nil
}

func TestTodosAreSegregatedByPhoneNumber(t *testing.T) {
	_, _, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)

	first, _ := service.handleMessage("+12065550101", "call mom", now)
	second, _ := service.handleMessage("+12065550102", "do dishes", now)

	if first != "0: call mom" {
		t.Fatalf("unexpected first response: %q", first)
	}
	if second != "0: do dishes" {
		t.Fatalf("unexpected second response: %q", second)
	}
	if got := service.todoList("+12065550101"); got != "0: call mom" {
		t.Fatalf("first user's list = %q", got)
	}
	if got := service.todoList("+12065550102"); got != "0: do dishes" {
		t.Fatalf("second user's list = %q", got)
	}
}

func TestMessageAddsTodoAndReplyContainsCompleteUpdatedList(t *testing.T) {
	_, _, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)
	phone := "+12065550101"
	service.handleMessage(phone, "first", now)

	response, _ := service.handleMessage(phone, "second", now)

	if response != "0: first\n1: second" {
		t.Fatalf("updated list response = %q", response)
	}
}

func TestCompletionRemovesTodoWithoutReusingNumber(t *testing.T) {
	_, _, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)
	phone := "+12065550101"
	service.handleMessage(phone, "first", now)
	service.handleMessage(phone, "second", now)

	completed, _ := service.handleMessage(phone, "0", now)
	added, _ := service.handleMessage(phone, "third", now)

	if completed != "1: second" {
		t.Fatalf("completion response = %q", completed)
	}
	if added != "1: second\n2: third" {
		t.Fatalf("add response = %q", added)
	}
}

func TestCompletionAllowsSurroundingWhitespace(t *testing.T) {
	_, _, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)
	phone := "+12065550101"
	service.handleMessage(phone, "first", now)

	response, _ := service.handleMessage(phone, " 0 ", now)

	if response != "No todos." {
		t.Fatalf("response = %q", response)
	}
	if got := service.todoList(phone); got != "No todos." {
		t.Fatalf("todo was not completed: %q", got)
	}
}

func TestGarbageCollectRenumbersTodosWithoutReordering(t *testing.T) {
	_, store, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)
	phone := "+12065550101"
	service.handleMessage(phone, "first", now)
	service.handleMessage(phone, "second", now)
	service.handleMessage(phone, "third", now)
	service.handleMessage(phone, "1", now)

	response, _ := service.handleMessage(phone, "gc", now)

	if response != "0: first\n1: third" {
		t.Fatalf("gc response = %q", response)
	}
	if store.state.Users[phone].NextTodoNumber != 2 {
		t.Fatalf("next todo number = %d", store.state.Users[phone].NextTodoNumber)
	}
	added, _ := service.handleMessage(phone, "fourth", now)
	if added != "0: first\n1: third\n2: fourth" {
		t.Fatalf("post-gc add response = %q", added)
	}
}

func TestGarbageCollectEmptyListResetsNextNumber(t *testing.T) {
	_, store, service := testService(t)
	phone := "+12065550101"
	store.state.Users[phone].NextTodoNumber = 9

	response, _ := service.handleMessage(phone, "gc", time.Now())

	if response != "No todos." {
		t.Fatalf("gc response = %q", response)
	}
	if store.state.Users[phone].NextTodoNumber != 0 {
		t.Fatalf("next todo number = %d", store.state.Users[phone].NextTodoNumber)
	}
}

func TestAddPrefixHasNoSpecialMeaning(t *testing.T) {
	_, _, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)

	response, _ := service.handleMessage("+12065550101", "add call mom", now)

	if response != "0: add call mom" {
		t.Fatalf("response = %q", response)
	}
}

func TestUnknownPhoneIsIgnored(t *testing.T) {
	_, _, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)

	response, shouldReply := service.handleMessage(
		"+12065550999",
		"steal data",
		now,
	)

	if shouldReply || response != "" {
		t.Fatalf("unknown phone received response %q", response)
	}
}

func TestDailyScheduleUsesLocalTimezone(t *testing.T) {
	_, _, service := testService(t)
	before := time.Date(2026, 8, 5, 14, 59, 0, 0, time.UTC)
	atTime := time.Date(2026, 8, 5, 15, 0, 0, 0, time.UTC)

	if containsUser(service.usersDue(before), "+12065550101") {
		t.Fatal("Los Angeles user was due before 08:00 local time")
	}
	if !containsUser(service.usersDue(atTime), "+12065550101") {
		t.Fatal("Los Angeles user was not due at 08:00 local time")
	}
}

func containsUser(users []*UserConfig, phone string) bool {
	for _, user := range users {
		if user.PhoneNumber == phone {
			return true
		}
	}
	return false
}

func TestDailyListIsSentOnlyOncePerLocalDay(t *testing.T) {
	config, store, service := testService(t)
	now := time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC)
	phone := "+12065550101"
	service.handleMessage(phone, "call mom", now)
	router := &fakeRouter{}
	app := newApp(config, store, router)

	if err := app.sendDailyLists(context.Background(), now); err != nil {
		t.Fatalf("first daily send: %v", err)
	}
	if err := app.sendDailyLists(context.Background(), now); err != nil {
		t.Fatalf("second daily send: %v", err)
	}

	if len(router.sent) != 2 {
		t.Fatalf("sent %d messages, want one per user", len(router.sent))
	}
	if router.sent[0] != (sentMessage{number: phone, message: "0: call mom"}) {
		t.Fatalf("first daily message = %#v", router.sent[0])
	}
	if router.sent[1] != (sentMessage{number: "+12065550102", message: "No todos."}) {
		t.Fatalf("second daily message = %#v", router.sent[1])
	}
}

func TestFirstPollBaselinesExistingInbox(t *testing.T) {
	config, store, service := testService(t)
	router := &fakeRouter{messages: []Message{
		{
			ID:      "7",
			ModemID: "1-1",
			Sender:  "+12065550101",
			Message: "historical task",
			Date:    "2026-08-05 09:00:00",
		},
	}}
	app := newApp(config, store, router)

	if err := app.processIncoming(context.Background(), time.Now()); err != nil {
		t.Fatalf("process inbox: %v", err)
	}

	if got := service.todoList("+12065550101"); got != "No todos." {
		t.Fatalf("historical command was processed: %q", got)
	}
	if len(router.sent) != 0 {
		t.Fatalf("sent %d replies during baseline", len(router.sent))
	}
	if !store.state.Initialized {
		t.Fatal("state was not initialized")
	}
}

func TestNewInboundCommandIsAppliedOnce(t *testing.T) {
	config, store, service := testService(t)
	store.state.Initialized = true
	if err := store.save(); err != nil {
		t.Fatalf("save initialized state: %v", err)
	}
	router := &fakeRouter{messages: []Message{
		{
			ID:      "8",
			ModemID: "1-1",
			Sender:  "+12065550101",
			Message: "new task",
			Date:    "2026-08-05 10:00:00",
		},
	}}
	app := newApp(config, store, router)

	if err := app.processIncoming(context.Background(), time.Now()); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	if err := app.processIncoming(context.Background(), time.Now()); err != nil {
		t.Fatalf("second poll: %v", err)
	}

	if got := service.todoList("+12065550101"); got != "0: new task" {
		t.Fatalf("todo list = %q", got)
	}
	if len(router.sent) != 1 || router.sent[0].message != "0: new task" {
		t.Fatalf("sent replies = %#v", router.sent)
	}
}

func TestStateRemainsValidJSONAfterSave(t *testing.T) {
	_, store, service := testService(t)
	service.handleMessage(
		"+12065550101",
		"saved",
		time.Date(2026, 8, 5, 17, 0, 0, 0, time.UTC),
	)
	if err := store.save(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	data, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var decoded State
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode saved state: %v", err)
	}
	if decoded.Users["+12065550101"].Todos[0].Text != "saved" {
		t.Fatalf("saved state = %#v", decoded)
	}
}

func TestLongSMSIsSplitIntoNumberedChunks(t *testing.T) {
	chunks := chunkSMS(strings.Repeat("x", 900))

	if len(chunks) != 3 {
		t.Fatalf("got %d chunks", len(chunks))
	}
	if !strings.HasPrefix(chunks[0], "(1/3) ") || !strings.HasPrefix(chunks[2], "(3/3) ") {
		t.Fatalf("chunk labels = %#v", chunks)
	}
	for _, chunk := range chunks {
		if len([]rune(chunk)) >= 460 {
			t.Fatalf("chunk has %d characters", len([]rune(chunk)))
		}
	}
}

func TestExampleConfigurationIsValid(t *testing.T) {
	config, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatalf("load example config: %v", err)
	}
	if config.Router.URL != "https://192.168.8.221" {
		t.Fatalf("router URL = %q", config.Router.URL)
	}
	if config.Users[0].TimezoneName != "America/Los_Angeles" {
		t.Fatalf("timezone = %q", config.Users[0].TimezoneName)
	}
}

func TestLoadsPythonGeneratedState(t *testing.T) {
	config := testConfig(t, filepath.Join(t.TempDir(), "state.json"))
	pythonState := `{
  "initialized": true,
  "processed_messages": ["1-1|2|date|+12065550101"],
  "users": {
    "+12065550101": {
      "last_daily_attempt": null,
      "last_daily_sent": null,
      "next_todo_number": 2,
      "todos": [
        {"created_at": "2026-08-05T17:35:42Z", "number": 1, "text": "kept"}
      ]
    }
  },
  "version": 1
}`
	if err := os.WriteFile(config.StateFile, []byte(pythonState), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	store, err := loadState(config.StateFile, config)
	if err != nil {
		t.Fatalf("load Python state: %v", err)
	}

	if store.state.Users["+12065550101"].Todos[0].Text != "kept" {
		t.Fatalf("state was not preserved: %#v", store.state)
	}
	if store.state.Users["+12065550101"].NextTodoNumber != 2 {
		t.Fatalf("next number was not preserved")
	}
}
