package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"
	"unicode/utf8"
)

const (
	stateVersion        = 1
	maxProcessedMessage = 1000
	smsChunkSize        = 430
)

var (
	phonePattern  = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
	numberPattern = regexp.MustCompile(`^[0-9]+$`)
	// Carrier failures can arrive with the intended recipient as the sender.
	// Match the whole notice so ordinary todos discussing SMS still work.
	messageBlockingPattern = regexp.MustCompile(`(?i)^(?:free\s*msg:\s*)?(?:(?:receiver|sender)\s+\+?[0-9]+\s+)?unable to (?:send|receive) message\s*[-–—]\s*message blocking is active\.?$`)
)

type RouterConfig struct {
	URL         string `json:"url"`
	Username    string `json:"username"`
	PasswordEnv string `json:"password_env"`
	ModemID     string `json:"modem_id"`
	VerifyTLS   bool   `json:"verify_tls"`
}

type UserConfig struct {
	PhoneNumber  string `json:"phone_number"`
	DailyTime    string `json:"daily_time"`
	TimezoneName string `json:"timezone"`

	dailyHour   int
	dailyMinute int
	location    *time.Location
}

type Config struct {
	Router              RouterConfig `json:"router"`
	StateFile           string       `json:"state_file"`
	PollIntervalSeconds int          `json:"poll_interval_seconds"`
	MaxTodoLength       int          `json:"max_todo_length"`
	Users               []UserConfig `json:"users"`

	usersByPhone map[string]*UserConfig
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("parse configuration JSON: %w", err)
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return &config, nil
}

func (c *Config) validate() error {
	if c.Router.URL == "" {
		return errors.New("router.url must be a non-empty string")
	}
	parsedURL, err := url.Parse(c.Router.URL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" {
		return errors.New("router.url must be a valid https:// URL")
	}
	c.Router.URL = strings.TrimRight(c.Router.URL, "/")
	if c.Router.Username == "" {
		return errors.New("router.username must be a non-empty string")
	}
	if c.Router.PasswordEnv == "" {
		c.Router.PasswordEnv = "RUT241_PASSWORD"
	}
	if c.Router.ModemID == "" {
		c.Router.ModemID = "1-1"
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/teltonika-rut241-sms-todo/state.json"
	}
	if c.PollIntervalSeconds < 1 {
		return errors.New("poll_interval_seconds must be a positive integer")
	}
	if c.MaxTodoLength < 1 {
		return errors.New("max_todo_length must be a positive integer")
	}
	if len(c.Users) == 0 {
		return errors.New("users must be a non-empty array")
	}

	c.usersByPhone = make(map[string]*UserConfig, len(c.Users))
	for index := range c.Users {
		user := &c.Users[index]
		if !phonePattern.MatchString(user.PhoneNumber) {
			return fmt.Errorf("users[%d].phone_number must use E.164 format", index)
		}
		if _, exists := c.usersByPhone[user.PhoneNumber]; exists {
			return fmt.Errorf("duplicate user phone number: %s", user.PhoneNumber)
		}
		parsedTime, err := time.Parse("15:04", user.DailyTime)
		if err != nil {
			return fmt.Errorf("users[%d].daily_time must use HH:MM format", index)
		}
		location, err := time.LoadLocation(user.TimezoneName)
		if err != nil {
			return fmt.Errorf("users[%d].timezone is unknown: %w", index, err)
		}
		user.dailyHour = parsedTime.Hour()
		user.dailyMinute = parsedTime.Minute()
		user.location = location
		c.usersByPhone[user.PhoneNumber] = user
	}
	return nil
}

type Todo struct {
	Number    int    `json:"number"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

type UserState struct {
	NextTodoNumber   int     `json:"next_todo_number"`
	Todos            []Todo  `json:"todos"`
	LastDailyAttempt *string `json:"last_daily_attempt"`
	LastDailySent    *string `json:"last_daily_sent"`
}

type State struct {
	Version           int                   `json:"version"`
	Initialized       bool                  `json:"initialized"`
	Users             map[string]*UserState `json:"users"`
	ProcessedMessages []string              `json:"processed_messages"`
}

type StateStore struct {
	path  string
	state *State
}

func loadState(path string, config *Config) (*StateStore, error) {
	state := &State{
		Version:           stateVersion,
		Users:             make(map[string]*UserState),
		ProcessedMessages: []string{},
	}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, state); err != nil {
			return nil, fmt.Errorf("parse state JSON: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read state: %w", err)
	}

	if state.Version != stateVersion {
		return nil, fmt.Errorf(
			"unsupported state version %d; expected %d",
			state.Version,
			stateVersion,
		)
	}
	if state.Users == nil {
		state.Users = make(map[string]*UserState)
	}
	if state.ProcessedMessages == nil {
		state.ProcessedMessages = []string{}
	}
	for _, user := range config.Users {
		if _, exists := state.Users[user.PhoneNumber]; !exists {
			state.Users[user.PhoneNumber] = &UserState{Todos: []Todo{}}
		}
	}
	return &StateStore{path: path, state: state}, nil
}

func (s *StateStore) save() error {
	directory := filepath.Dir(s.path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}

	temporary, err := os.CreateTemp(directory, ".state.*.json")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set state permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(s.state); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if err := os.Rename(temporaryName, s.path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	removeTemporary = false
	return nil
}

func (s *StateStore) hasProcessed(key string) bool {
	for _, existing := range s.state.ProcessedMessages {
		if existing == key {
			return true
		}
	}
	return false
}

func (s *StateStore) markProcessed(key string) {
	if s.hasProcessed(key) {
		return
	}
	s.state.ProcessedMessages = append(s.state.ProcessedMessages, key)
	if len(s.state.ProcessedMessages) > maxProcessedMessage {
		start := len(s.state.ProcessedMessages) - maxProcessedMessage
		s.state.ProcessedMessages = s.state.ProcessedMessages[start:]
	}
}

type TodoService struct {
	config *Config
	store  *StateStore
}

func newTodoService(config *Config, store *StateStore) *TodoService {
	return &TodoService{config: config, store: store}
}

func (s *TodoService) handleMessage(
	sender string,
	message string,
	now time.Time,
) (string, bool) {
	if _, configured := s.config.usersByPhone[sender]; !configured {
		return "", false
	}
	if isCarrierFailure(message) {
		return "", false
	}
	userState := s.store.state.Users[sender]
	message = strings.TrimSpace(message)

	if message == "gc" {
		for index := range userState.Todos {
			userState.Todos[index].Number = index
		}
		userState.NextTodoNumber = len(userState.Todos)
		return s.todoList(sender), true
	}

	if numberPattern.MatchString(message) {
		number, err := strconv.Atoi(message)
		if err != nil {
			return fmt.Sprintf("Todo %s was not found.", message), true
		}
		for index, todo := range userState.Todos {
			if todo.Number == number {
				userState.Todos = append(
					userState.Todos[:index],
					userState.Todos[index+1:]...,
				)
				return s.todoList(sender), true
			}
		}
		return fmt.Sprintf("Todo %d was not found.", number), true
	}

	if message == "" {
		return "Send todo text to add it, a number to complete it, or gc to renumber the list.", true
	}
	if utf8.RuneCountInString(message) > s.config.MaxTodoLength {
		return fmt.Sprintf(
			"Todo is too long. Maximum length is %d characters.",
			s.config.MaxTodoLength,
		), true
	}
	number := userState.NextTodoNumber
	userState.NextTodoNumber++
	userState.Todos = append(userState.Todos, Todo{
		Number:    number,
		Text:      message,
		CreatedAt: now.UTC().Format(time.RFC3339Nano),
	})
	return s.todoList(sender), true
}

func isCarrierFailure(message string) bool {
	return messageBlockingPattern.MatchString(strings.TrimSpace(message))
}

func (s *TodoService) todoList(phone string) string {
	todos := s.store.state.Users[phone].Todos
	if len(todos) == 0 {
		return "No todos."
	}
	lines := make([]string, 0, len(todos))
	for _, todo := range todos {
		lines = append(lines, fmt.Sprintf("%d: %s", todo.Number, todo.Text))
	}
	return strings.Join(lines, "\n")
}

func (s *TodoService) usersDue(now time.Time) []*UserConfig {
	var due []*UserConfig
	for index := range s.config.Users {
		user := &s.config.Users[index]
		localNow := now.In(user.location)
		localDate := localNow.Format("2006-01-02")
		state := s.store.state.Users[user.PhoneNumber]
		alreadyAttempted := state.LastDailyAttempt != nil &&
			*state.LastDailyAttempt == localDate
		currentMinutes := localNow.Hour()*60 + localNow.Minute()
		dueMinutes := user.dailyHour*60 + user.dailyMinute
		if currentMinutes >= dueMinutes && !alreadyAttempted {
			due = append(due, user)
		}
	}
	return due
}

type APIError struct {
	Error string `json:"error"`
}

type APIEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Errors  []APIError      `json:"errors"`
}

func (e APIEnvelope) errorMessage(fallback string) string {
	var messages []string
	for _, apiError := range e.Errors {
		if apiError.Error != "" {
			messages = append(messages, apiError.Error)
		}
	}
	if len(messages) > 0 {
		return strings.Join(messages, "; ")
	}
	return fallback
}

type Message struct {
	ID      string `json:"id"`
	ModemID string `json:"modem_id"`
	Sender  string `json:"sender"`
	Message string `json:"message"`
	Date    string `json:"date"`
}

func (m Message) key() string {
	return strings.Join([]string{m.ModemID, m.ID, m.Date, m.Sender}, "|")
}

type Router interface {
	GetMessages(context.Context) ([]Message, error)
	SendSMS(context.Context, string, string) error
}

type RUT241Client struct {
	baseURL  string
	username string
	password string
	modemID  string
	token    string
	client   *http.Client
}

func newRUT241Client(
	config RouterConfig,
	password string,
) *RUT241Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: !config.VerifyTLS,
	}
	return &RUT241Client{
		baseURL:  config.URL,
		username: config.Username,
		password: password,
		modemID:  config.ModemID,
		client: &http.Client{
			Timeout:   20 * time.Second,
			Transport: transport,
		},
	}
}

func (c *RUT241Client) authenticate(ctx context.Context) error {
	payload := map[string]string{
		"username": c.username,
		"password": c.password,
	}
	envelope, _, err := c.request(ctx, http.MethodPost, "/api/login", payload, false)
	if err != nil {
		return err
	}
	if !envelope.Success {
		return errors.New(envelope.errorMessage("authentication failed"))
	}
	var data struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil || data.Token == "" {
		return errors.New("authentication response did not contain a token")
	}
	c.token = data.Token
	return nil
}

func (c *RUT241Client) GetMessages(ctx context.Context) ([]Message, error) {
	envelope, err := c.authenticatedRequest(ctx, http.MethodGet, "/api/messages/status", nil)
	if err != nil {
		return nil, err
	}
	if !envelope.Success {
		return nil, errors.New(envelope.errorMessage("could not retrieve messages"))
	}
	var messages []Message
	if err := json.Unmarshal(envelope.Data, &messages); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	return messages, nil
}

func (c *RUT241Client) SendSMS(
	ctx context.Context,
	number string,
	message string,
) error {
	payload := map[string]any{
		"data": map[string]string{
			"number":  number,
			"message": message,
			"modem":   c.modemID,
		},
	}
	envelope, err := c.authenticatedRequest(
		ctx,
		http.MethodPost,
		"/api/messages/actions/send",
		payload,
	)
	if err != nil {
		return err
	}
	if !envelope.Success {
		return errors.New(envelope.errorMessage("SMS send failed"))
	}
	return nil
}

func (c *RUT241Client) authenticatedRequest(
	ctx context.Context,
	method string,
	path string,
	payload any,
) (APIEnvelope, error) {
	if c.token == "" {
		if err := c.authenticate(ctx); err != nil {
			return APIEnvelope{}, err
		}
	}
	envelope, status, err := c.request(ctx, method, path, payload, true)
	if err != nil {
		return APIEnvelope{}, err
	}
	if status != http.StatusUnauthorized {
		return envelope, nil
	}

	c.token = ""
	if err := c.authenticate(ctx); err != nil {
		return APIEnvelope{}, err
	}
	envelope, status, err = c.request(ctx, method, path, payload, true)
	if err != nil {
		return APIEnvelope{}, err
	}
	if status == http.StatusUnauthorized {
		return APIEnvelope{}, errors.New("RUT241 authentication was rejected")
	}
	return envelope, nil
}

func (c *RUT241Client) request(
	ctx context.Context,
	method string,
	path string,
	payload any,
	authenticated bool,
) (APIEnvelope, int, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return APIEnvelope{}, 0, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return APIEnvelope{}, 0, fmt.Errorf("create request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if authenticated {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return APIEnvelope{}, 0, fmt.Errorf("RUT241 request failed: %w", err)
	}
	defer response.Body.Close()

	var envelope APIEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return APIEnvelope{}, response.StatusCode, fmt.Errorf(
			"decode RUT241 response: %w",
			err,
		)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode != http.StatusUnauthorized {
			return envelope, response.StatusCode, fmt.Errorf(
				"RUT241 returned HTTP %d: %s",
				response.StatusCode,
				envelope.errorMessage(response.Status),
			)
		}
	}
	return envelope, response.StatusCode, nil
}

func chunkSMS(text string) []string {
	runes := []rune(text)
	if len(runes) <= smsChunkSize {
		return []string{text}
	}
	var rawChunks []string
	for start := 0; start < len(runes); start += smsChunkSize {
		end := start + smsChunkSize
		if end > len(runes) {
			end = len(runes)
		}
		rawChunks = append(rawChunks, string(runes[start:end]))
	}
	chunks := make([]string, 0, len(rawChunks))
	for index, chunk := range rawChunks {
		chunks = append(chunks, fmt.Sprintf("(%d/%d) %s", index+1, len(rawChunks), chunk))
	}
	return chunks
}

type App struct {
	config  *Config
	store   *StateStore
	service *TodoService
	router  Router
}

func newApp(config *Config, store *StateStore, router Router) *App {
	return &App{
		config:  config,
		store:   store,
		service: newTodoService(config, store),
		router:  router,
	}
}

func (a *App) runOnce(ctx context.Context, now time.Time) error {
	if err := a.processIncoming(ctx, now); err != nil {
		return err
	}
	return a.sendDailyLists(ctx, now)
}

func (a *App) processIncoming(ctx context.Context, now time.Time) error {
	messages, err := a.router.GetMessages(ctx)
	if err != nil {
		return err
	}
	sort.Slice(messages, func(left, right int) bool {
		if messages[left].Date == messages[right].Date {
			return messages[left].ID < messages[right].ID
		}
		return messages[left].Date < messages[right].Date
	})

	if !a.store.state.Initialized {
		for _, message := range messages {
			a.store.markProcessed(message.key())
		}
		a.store.state.Initialized = true
		if err := a.store.save(); err != nil {
			return err
		}
		log.Printf("initialized inbox baseline with %d existing messages", len(messages))
		return nil
	}

	for _, message := range messages {
		key := message.key()
		if a.store.hasProcessed(key) {
			continue
		}
		reply, shouldReply := a.service.handleMessage(
			message.Sender,
			message.Message,
			now,
		)
		a.store.markProcessed(key)
		if err := a.store.save(); err != nil {
			return err
		}
		log.Printf("processed message %s from %s", message.ID, message.Sender)

		if !shouldReply {
			if isCarrierFailure(message.Message) {
				log.Printf("ignored carrier message-blocking notice from %s; check carrier SMS service", message.Sender)
			} else {
				log.Printf("ignored message from unconfigured number %s", message.Sender)
			}
			continue
		}
		if err := a.sendText(ctx, message.Sender, reply); err != nil {
			log.Printf("failed to send command response to %s: %v", message.Sender, err)
		}
	}
	return nil
}

func (a *App) sendDailyLists(ctx context.Context, now time.Time) error {
	for _, user := range a.service.usersDue(now) {
		localDate := now.In(user.location).Format("2006-01-02")
		userState := a.store.state.Users[user.PhoneNumber]
		userState.LastDailyAttempt = stringPointer(localDate)
		if err := a.store.save(); err != nil {
			return err
		}

		if err := a.sendText(ctx, user.PhoneNumber, a.service.todoList(user.PhoneNumber)); err != nil {
			log.Printf("daily todo SMS failed for %s: %v", user.PhoneNumber, err)
			continue
		}
		userState.LastDailySent = stringPointer(localDate)
		if err := a.store.save(); err != nil {
			return err
		}
		log.Printf("sent daily todo list to %s", user.PhoneNumber)
	}
	return nil
}

func (a *App) sendText(ctx context.Context, number string, text string) error {
	for _, chunk := range chunkSMS(text) {
		if err := a.router.SendSMS(ctx, number, chunk); err != nil {
			return err
		}
	}
	return nil
}

func stringPointer(value string) *string {
	return &value
}

func usage(flags *flag.FlagSet) {
	fmt.Fprintf(flags.Output(), "Usage: %s [options] [run|once|check-config]\n", flags.Name())
	flags.PrintDefaults()
}

func main() {
	flags := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ExitOnError)
	configPath := flags.String("config", "config.json", "path to JSON configuration")
	flags.Usage = func() { usage(flags) }
	_ = flags.Parse(os.Args[1:])

	command := "run"
	if flags.NArg() > 0 {
		command = flags.Arg(0)
	}
	if flags.NArg() > 1 || (command != "run" && command != "once" && command != "check-config") {
		flags.Usage()
		os.Exit(2)
	}

	config, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}
	if command == "check-config" {
		fmt.Printf("Configuration is valid: %s\n", *configPath)
		return
	}

	password := os.Getenv(config.Router.PasswordEnv)
	if password == "" {
		log.Fatalf("required environment variable is not set: %s", config.Router.PasswordEnv)
	}
	store, err := loadState(config.StateFile, config)
	if err != nil {
		log.Fatalf("state error: %v", err)
	}
	router := newRUT241Client(config.Router, password)
	app := newApp(config, store, router)

	if command == "once" {
		if err := app.runOnce(context.Background(), time.Now()); err != nil {
			log.Fatalf("operation failed: %v", err)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("Teltonika RUT241 SMS Todo started")

	for {
		started := time.Now()
		if err := app.runOnce(ctx, started); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("router operation failed; will retry: %v", err)
		}
		wait := time.Duration(config.PollIntervalSeconds)*time.Second - time.Since(started)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			log.Printf("Teltonika RUT241 SMS Todo stopped")
			return
		case <-timer.C:
		}
	}
}
