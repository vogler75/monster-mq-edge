package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// ClientConfig holds the broker GraphQL connection settings.
type ClientConfig struct {
	URL      string
	Username string
	Password string
	Token    string
	Timeout  time.Duration
	JSONMode bool
}

// LoadDotEnv loads key=value pairs from a .env file into os.Environ if not already set.
func LoadDotEnv(filePath string) {
	if filePath == "" {
		filePath = ".env"
	}
	file, err := os.Open(filePath)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, "\"'")
			if os.Getenv(key) == "" {
				os.Setenv(key, val)
			}
		}
	}
}

// ResolveClientConfig merges CLI flags, environment variables, and defaults.
func ResolveClientConfig(flagURL, flagUser, flagPass, flagToken, envFile string, jsonMode bool) *ClientConfig {
	LoadDotEnv(envFile)

	url := flagURL
	if url == "" {
		url = os.Getenv("MQ_URL")
	}
	if url == "" {
		url = os.Getenv("GRAPHQL_URL")
	}
	if url == "" {
		url = "http://localhost:4000/graphql"
	}

	user := flagUser
	if user == "" {
		user = os.Getenv("MQ_USER")
	}
	if user == "" {
		user = os.Getenv("GRAPHQL_USER")
	}

	pass := flagPass
	if pass == "" {
		pass = os.Getenv("MQ_PASS")
	}
	if pass == "" {
		pass = os.Getenv("GRAPHQL_PASS")
	}

	token := flagToken
	if token == "" {
		token = os.Getenv("MQ_TOKEN")
	}
	if token == "" {
		token = os.Getenv("GRAPHQL_TOKEN")
	}

	return &ClientConfig{
		URL:      url,
		Username: user,
		Password: pass,
		Token:    token,
		Timeout:  15 * time.Second,
		JSONMode: jsonMode,
	}
}

// Client wraps HTTP interactions with the MonsterMQ GraphQL endpoint.
type Client struct {
	cfg        *ClientConfig
	httpClient *http.Client
	token      string
}

// NewClient initializes a Client and performs auto-login if credentials are set.
func NewClient(cfg *ClientConfig) *Client {
	c := &Client{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: cfg.Timeout,
		},
		token: cfg.Token,
	}
	return c
}

// EnsureAuthenticated performs login if token is empty but credentials exist.
func (c *Client) EnsureAuthenticated(ctx context.Context) error {
	if c.token != "" || c.cfg.Username == "" {
		return nil
	}

	loginQuery := `
		mutation Login($username: String!, $password: String!) {
			login(username: $username, password: $password) {
				success
				message
				token
			}
		}
	`
	var res struct {
		Data struct {
			Login struct {
				Success bool   `json:"success"`
				Message string `json:"message"`
				Token   string `json:"token"`
			} `json:"login"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	err := c.rawDo(ctx, loginQuery, map[string]any{
		"username": c.cfg.Username,
		"password": c.cfg.Password,
	}, &res, false)

	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	if len(res.Errors) > 0 {
		return fmt.Errorf("login GraphQL error: %s", res.Errors[0].Message)
	}
	if !res.Data.Login.Success {
		return fmt.Errorf("login failed: %s", res.Data.Login.Message)
	}

	c.token = res.Data.Login.Token
	return nil
}

func (c *Client) DoQuery(ctx context.Context, query string, variables map[string]any, resultPtr any) error {
	if err := c.EnsureAuthenticated(ctx); err != nil {
		return err
	}
	return c.rawDo(ctx, query, variables, resultPtr, true)
}

func (c *Client) rawDo(ctx context.Context, query string, variables map[string]any, resultPtr any, attachAuth bool) error {
	payload := map[string]any{
		"query": query,
	}
	if len(variables) > 0 {
		payload["variables"] = variables
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.cfg.URL, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	if attachAuth && c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBytes))
	}

	return json.Unmarshal(respBytes, resultPtr)
}
