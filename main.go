package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/tasks/v1"
)

// pendingAuth holds state for an in-progress OAuth authorization.
type pendingAuth struct {
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
	GoogleTokens  *oauth2.Token // set after Google callback
	AuthCode      string        // our issued auth code
	ExpiresAt     time.Time
}

// tokenEntry maps an issued access token to Google credentials.
type tokenEntry struct {
	GoogleRefreshToken string
	ClientID           string
	ExpiresAt          time.Time
}

// registeredClient holds dynamic client registration info.
type registeredClient struct {
	ClientID     string   `json:"client_id"`
	ClientSecret string   `json:"client_secret,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	Name         string   `json:"client_name,omitempty"`
}

var (
	// In-memory stores (reference implementation only).
	pendingAuths      = map[string]*pendingAuth{}      // keyed by state
	authCodes         = map[string]*pendingAuth{}      // keyed by auth code
	accessTokens      = map[string]*tokenEntry{}       // keyed by access token
	registeredClients = map[string]*registeredClient{} // keyed by client_id
	mu                sync.Mutex
)

func main() {
	clientID := os.Getenv("GOOGLE_CLIENT_ID")
	clientSecret := os.Getenv("GOOGLE_CLIENT_SECRET")
	if clientID == "" || clientSecret == "" {
		log.Fatal("GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET environment variables are required")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	baseURL := os.Getenv("BASE_URL")
	if baseURL == "" {
		baseURL = "http://localhost:" + port
	}

	googleOAuth := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  baseURL + "/google/callback",
		Scopes:       []string{tasks.TasksScope},
	}

	mux := http.NewServeMux()

	// RFC 8414: OAuth 2.0 Authorization Server Metadata.
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                baseURL,
			"authorization_endpoint":                baseURL + "/authorize",
			"token_endpoint":                        baseURL + "/token",
			"registration_endpoint":                 baseURL + "/register",
			"response_types_supported":              []string{"code"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post"},
			"code_challenge_methods_supported":      []string{"S256"},
			"scopes_supported":                      []string{tasks.TasksScope},
		})
	})

	// RFC 7591: Dynamic Client Registration.
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
			ClientName   string   `json:"client_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid_request", "Invalid JSON body", http.StatusBadRequest)
			return
		}

		id, _ := randomHex(16)
		client := &registeredClient{
			ClientID:     id,
			RedirectURIs: req.RedirectURIs,
			Name:         req.ClientName,
		}

		mu.Lock()
		registeredClients[id] = client
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(client)
	})

	// OAuth 2.1 Authorization Endpoint.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cID := q.Get("client_id")
		redirectURI := q.Get("redirect_uri")
		state := q.Get("state")
		codeChallenge := q.Get("code_challenge")
		codeChallengeMethod := q.Get("code_challenge_method")

		if cID == "" || redirectURI == "" || codeChallenge == "" {
			jsonError(w, "invalid_request", "Missing required parameters: client_id, redirect_uri, code_challenge", http.StatusBadRequest)
			return
		}
		if codeChallengeMethod != "" && codeChallengeMethod != "S256" {
			jsonError(w, "invalid_request", "Only S256 code_challenge_method is supported", http.StatusBadRequest)
			return
		}

		// Generate internal state for the Google OAuth leg.
		googleState, _ := randomHex(16)

		mu.Lock()
		pendingAuths[googleState] = &pendingAuth{
			ClientID:      cID,
			RedirectURI:   redirectURI,
			State:         state,
			CodeChallenge: codeChallenge,
			ExpiresAt:     time.Now().Add(10 * time.Minute),
		}
		mu.Unlock()

		// Redirect to Google consent screen.
		url := googleOAuth.AuthCodeURL(googleState, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
		http.Redirect(w, r, url, http.StatusFound)
	})

	// Google OAuth callback: exchanges Google code, issues our auth code, redirects back to MCP client.
	mux.HandleFunc("GET /google/callback", func(w http.ResponseWriter, r *http.Request) {
		googleState := r.URL.Query().Get("state")
		code := r.URL.Query().Get("code")

		mu.Lock()
		pending, ok := pendingAuths[googleState]
		if ok {
			delete(pendingAuths, googleState)
		}
		mu.Unlock()

		if !ok || time.Now().After(pending.ExpiresAt) {
			http.Error(w, "Invalid or expired state", http.StatusBadRequest)
			return
		}

		// Exchange Google auth code for tokens.
		token, err := googleOAuth.Exchange(r.Context(), code)
		if err != nil {
			http.Error(w, "Google token exchange failed: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Generate our authorization code.
		authCode, _ := randomHex(32)
		pending.GoogleTokens = token
		pending.AuthCode = authCode

		mu.Lock()
		authCodes[authCode] = pending
		mu.Unlock()

		// Redirect back to MCP client's redirect_uri with our auth code.
		redirectURL := pending.RedirectURI + "?code=" + authCode
		if pending.State != "" {
			redirectURL += "&state=" + pending.State
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
	})

	// OAuth 2.1 Token Endpoint.
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			jsonError(w, "invalid_request", "Invalid form body", http.StatusBadRequest)
			return
		}

		grantType := r.FormValue("grant_type")
		switch grantType {
		case "authorization_code":
			handleAuthCodeGrant(w, r, googleOAuth)
		case "refresh_token":
			handleRefreshTokenGrant(w, r, googleOAuth)
		default:
			jsonError(w, "unsupported_grant_type", "Supported: authorization_code, refresh_token", http.StatusBadRequest)
		}
	})

	// MCP streamable HTTP endpoint.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		token := extractBearerToken(r)

		mu.Lock()
		entry, ok := accessTokens[token]
		mu.Unlock()

		if !ok || time.Now().After(entry.ExpiresAt) {
			return nil
		}

		svc, err := newTasksService(r.Context(), googleOAuth, entry.GoogleRefreshToken)
		if err != nil {
			log.Printf("Failed to create Tasks service: %v", err)
			return nil
		}

		server := mcp.NewServer(
			&mcp.Implementation{Name: "agentic-layer/mcp-server-gtasks", Version: "0.1.0"},
			nil,
		)
		registerTools(server, svc)
		registerResources(server, svc)
		return server
	}, nil)

	// Wrap MCP handler: return 401 if no valid bearer token.
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		token := extractBearerToken(r)
		if token == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, baseURL))
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		mu.Lock()
		entry, ok := accessTokens[token]
		mu.Unlock()

		if !ok || time.Now().After(entry.ExpiresAt) {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		mcpHandler.ServeHTTP(w, r)
	})

	// RFC 9728: OAuth 2.0 Protected Resource Metadata.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resource":              baseURL + "/mcp",
			"authorization_servers": []string{baseURL},
			"scopes_supported":      []string{tasks.TasksScope},
		})
	})

	log.Printf("MCP server listening on :%s", port)
	log.Printf("  MCP endpoint:     %s/mcp", baseURL)
	log.Printf("  Auth metadata:    %s/.well-known/oauth-authorization-server", baseURL)
	log.Printf("  Resource metadata:%s/.well-known/oauth-protected-resource", baseURL)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

// handleAuthCodeGrant exchanges an authorization code for access + refresh tokens.
func handleAuthCodeGrant(w http.ResponseWriter, r *http.Request, _ *oauth2.Config) {
	code := r.FormValue("code")
	codeVerifier := r.FormValue("code_verifier")

	mu.Lock()
	pending, ok := authCodes[code]
	if ok {
		delete(authCodes, code)
	}
	mu.Unlock()

	if !ok {
		jsonError(w, "invalid_grant", "Invalid or expired authorization code", http.StatusBadRequest)
		return
	}

	// Validate PKCE: S256 verification.
	if !verifyPKCE(pending.CodeChallenge, codeVerifier) {
		jsonError(w, "invalid_grant", "PKCE verification failed", http.StatusBadRequest)
		return
	}

	// Issue our access token and refresh token.
	accessToken, _ := randomHex(32)
	refreshToken, _ := randomHex(32)
	expiresIn := 3600

	mu.Lock()
	accessTokens[accessToken] = &tokenEntry{
		GoogleRefreshToken: pending.GoogleTokens.RefreshToken,
		ClientID:           pending.ClientID,
		ExpiresAt:          time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	// Store refresh token mapping (reuse accessTokens map with long expiry).
	accessTokens[refreshToken] = &tokenEntry{
		GoogleRefreshToken: pending.GoogleTokens.RefreshToken,
		ClientID:           pending.ClientID,
		ExpiresAt:          time.Now().Add(90 * 24 * time.Hour),
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    expiresIn,
		"refresh_token": refreshToken,
	})
}

// handleRefreshTokenGrant exchanges a refresh token for a new access token.
func handleRefreshTokenGrant(w http.ResponseWriter, r *http.Request, _ *oauth2.Config) {
	refreshToken := r.FormValue("refresh_token")

	mu.Lock()
	entry, ok := accessTokens[refreshToken]
	mu.Unlock()

	if !ok || time.Now().After(entry.ExpiresAt) {
		jsonError(w, "invalid_grant", "Invalid or expired refresh token", http.StatusBadRequest)
		return
	}

	// Issue new access token.
	newAccessToken, _ := randomHex(32)
	expiresIn := 3600

	mu.Lock()
	accessTokens[newAccessToken] = &tokenEntry{
		GoogleRefreshToken: entry.GoogleRefreshToken,
		ClientID:           entry.ClientID,
		ExpiresAt:          time.Now().Add(time.Duration(expiresIn) * time.Second),
	}
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": newAccessToken,
		"token_type":   "Bearer",
		"expires_in":   expiresIn,
	})
}

// verifyPKCE validates S256 code challenge against verifier.
func verifyPKCE(challenge, verifier string) bool {
	if challenge == "" || verifier == "" {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(h[:])
	return computed == challenge
}

func jsonError(w http.ResponseWriter, code, description string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": description,
	})
}

func newTasksService(ctx context.Context, conf *oauth2.Config, refreshToken string) (*tasks.Service, error) {
	token := &oauth2.Token{RefreshToken: refreshToken}
	tokenSource := conf.TokenSource(ctx, token)
	return tasks.NewService(ctx, option.WithTokenSource(tokenSource))
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// --- Tool Definitions ---

type SearchInput struct {
	Query string `json:"query" jsonschema:"description=Search query"`
}

type ListInput struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"description=Cursor for pagination"`
}

type CreateInput struct {
	TaskListID string `json:"taskListId,omitempty" jsonschema:"description=Task list ID (defaults to @default)"`
	Title      string `json:"title" jsonschema:"description=Task title"`
	Notes      string `json:"notes,omitempty" jsonschema:"description=Task notes"`
	Due        string `json:"due,omitempty" jsonschema:"description=Due date (RFC 3339)"`
}

type UpdateInput struct {
	ID         string `json:"id" jsonschema:"description=Task ID"`
	TaskListID string `json:"taskListId,omitempty" jsonschema:"description=Task list ID (defaults to @default)"`
	Title      string `json:"title,omitempty" jsonschema:"description=Task title"`
	Notes      string `json:"notes,omitempty" jsonschema:"description=Task notes"`
	Status     string `json:"status,omitempty" jsonschema:"description=Task status (needsAction or completed),enum=needsAction|completed"`
	Due        string `json:"due,omitempty" jsonschema:"description=Due date (RFC 3339)"`
}

type DeleteInput struct {
	ID         string `json:"id" jsonschema:"description=Task ID"`
	TaskListID string `json:"taskListId" jsonschema:"description=Task list ID"`
}

type ClearInput struct {
	TaskListID string `json:"taskListId" jsonschema:"description=Task list ID"`
}

func registerTools(server *mcp.Server, svc *tasks.Service) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Search for a task in Google Tasks",
	}, searchHandler(svc))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list",
		Description: "List all tasks in Google Tasks",
	}, listHandler(svc))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "create",
		Description: "Create a new task in Google Tasks",
	}, createHandler(svc))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update",
		Description: "Update a task in Google Tasks",
	}, updateHandler(svc))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete",
		Description: "Delete a task in Google Tasks",
	}, deleteHandler(svc))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "clear",
		Description: "Clear completed tasks from a Google Tasks task list",
	}, clearHandler(svc))
}

func searchHandler(svc *tasks.Service) func(context.Context, *mcp.CallToolRequest, SearchInput) (*mcp.CallToolResult, struct{}, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, struct{}, error) {
		query := strings.ToLower(input.Query)
		taskLists, err := svc.Tasklists.List().Context(ctx).Do()
		if err != nil {
			return nil, struct{}{}, fmt.Errorf("list task lists: %w", err)
		}

		var matches []string
		for _, tl := range taskLists.Items {
			tasksResp, err := svc.Tasks.List(tl.Id).Context(ctx).Do()
			if err != nil {
				continue
			}
			for _, t := range tasksResp.Items {
				if strings.Contains(strings.ToLower(t.Title), query) ||
					strings.Contains(strings.ToLower(t.Notes), query) {
					matches = append(matches, formatTask(t))
				}
			}
		}

		text := "No tasks found matching your search."
		if len(matches) > 0 {
			text = strings.Join(matches, "\n\n")
		}
		return textResult(text), struct{}{}, nil
	}
}

func listHandler(svc *tasks.Service) func(context.Context, *mcp.CallToolRequest, ListInput) (*mcp.CallToolResult, struct{}, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ListInput) (*mcp.CallToolResult, struct{}, error) {
		taskLists, err := svc.Tasklists.List().Context(ctx).Do()
		if err != nil {
			return nil, struct{}{}, fmt.Errorf("list task lists: %w", err)
		}

		var lines []string
		for _, tl := range taskLists.Items {
			call := svc.Tasks.List(tl.Id).Context(ctx)
			if input.Cursor != "" {
				call = call.PageToken(input.Cursor)
			}
			tasksResp, err := call.Do()
			if err != nil {
				continue
			}
			if len(tasksResp.Items) > 0 {
				lines = append(lines, fmt.Sprintf("== Task List: %s ==", tl.Title))
				for _, t := range tasksResp.Items {
					lines = append(lines, formatTask(t))
				}
			}
		}

		text := "No tasks found."
		if len(lines) > 0 {
			text = strings.Join(lines, "\n\n")
		}
		return textResult(text), struct{}{}, nil
	}
}

func createHandler(svc *tasks.Service) func(context.Context, *mcp.CallToolRequest, CreateInput) (*mcp.CallToolResult, struct{}, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input CreateInput) (*mcp.CallToolResult, struct{}, error) {
		taskListID := input.TaskListID
		if taskListID == "" {
			taskListID = "@default"
		}

		task := &tasks.Task{
			Title: input.Title,
			Notes: input.Notes,
			Due:   input.Due,
		}

		created, err := svc.Tasks.Insert(taskListID, task).Context(ctx).Do()
		if err != nil {
			return nil, struct{}{}, fmt.Errorf("create task: %w", err)
		}
		return textResult("Task created:\n" + formatTask(created)), struct{}{}, nil
	}
}

func updateHandler(svc *tasks.Service) func(context.Context, *mcp.CallToolRequest, UpdateInput) (*mcp.CallToolResult, struct{}, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input UpdateInput) (*mcp.CallToolResult, struct{}, error) {
		taskListID := input.TaskListID
		if taskListID == "" {
			taskListID = "@default"
		}

		task := &tasks.Task{}
		if input.Title != "" {
			task.Title = input.Title
		}
		if input.Notes != "" {
			task.Notes = input.Notes
		}
		if input.Status != "" {
			task.Status = input.Status
		}
		if input.Due != "" {
			task.Due = input.Due
		}

		updated, err := svc.Tasks.Patch(taskListID, input.ID, task).Context(ctx).Do()
		if err != nil {
			return nil, struct{}{}, fmt.Errorf("update task: %w", err)
		}
		return textResult("Task updated:\n" + formatTask(updated)), struct{}{}, nil
	}
}

func deleteHandler(svc *tasks.Service) func(context.Context, *mcp.CallToolRequest, DeleteInput) (*mcp.CallToolResult, struct{}, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input DeleteInput) (*mcp.CallToolResult, struct{}, error) {
		err := svc.Tasks.Delete(input.TaskListID, input.ID).Context(ctx).Do()
		if err != nil {
			return nil, struct{}{}, fmt.Errorf("delete task: %w", err)
		}
		return textResult(fmt.Sprintf("Task %s deleted.", input.ID)), struct{}{}, nil
	}
}

func clearHandler(svc *tasks.Service) func(context.Context, *mcp.CallToolRequest, ClearInput) (*mcp.CallToolResult, struct{}, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, input ClearInput) (*mcp.CallToolResult, struct{}, error) {
		err := svc.Tasks.Clear(input.TaskListID).Context(ctx).Do()
		if err != nil {
			return nil, struct{}{}, fmt.Errorf("clear tasks: %w", err)
		}
		return textResult(fmt.Sprintf("Completed tasks cleared from list %s.", input.TaskListID)), struct{}{}, nil
	}
}

// --- Resources ---

func registerResources(server *mcp.Server, svc *tasks.Service) {
	server.AddResourceTemplate(
		&mcp.ResourceTemplate{
			URITemplate: "gtasks:///{taskId}",
			Name:        "Google Task",
			MIMEType:    "text/plain",
		},
		readResourceHandler(svc),
	)
}

func readResourceHandler(svc *tasks.Service) func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		taskID := strings.TrimPrefix(req.Params.URI, "gtasks:///")

		taskLists, err := svc.Tasklists.List().Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("list task lists: %w", err)
		}

		for _, tl := range taskLists.Items {
			t, err := svc.Tasks.Get(tl.Id, taskID).Context(ctx).Do()
			if err != nil {
				continue
			}

			details := formatTaskDetails(t)
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{
					{URI: req.Params.URI, Text: details, MIMEType: "text/plain"},
				},
			}, nil
		}

		return nil, fmt.Errorf("task %s not found", taskID)
	}
}

// --- Helpers ---

func textResult(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
	}
}

func formatTask(t *tasks.Task) string {
	completed := "N/A"
	if t.Completed != nil && *t.Completed != "" {
		completed = *t.Completed
	}
	due := "Not set"
	if t.Due != "" {
		due = t.Due
	}
	notes := "No notes"
	if t.Notes != "" {
		notes = t.Notes
	}
	status := "Unknown"
	if t.Status != "" {
		status = t.Status
	}
	updated := "Unknown"
	if t.Updated != "" {
		updated = t.Updated
	}

	return fmt.Sprintf("Title: %s\n  ID: %s\n  Status: %s\n  Due: %s\n  Notes: %s\n  Updated: %s\n  Completed: %s",
		t.Title, t.Id, status, due, notes, updated, completed)
}

func formatTaskDetails(t *tasks.Task) string {
	ptrVal := func(s *string, fallback string) string {
		if s != nil && *s != "" {
			return *s
		}
		return fallback
	}
	val := func(s string, fallback string) string {
		if s != "" {
			return s
		}
		return fallback
	}

	return strings.Join([]string{
		"Title: " + val(t.Title, "No title"),
		"Status: " + val(t.Status, "Unknown"),
		"Due: " + val(t.Due, "Not set"),
		"Notes: " + val(t.Notes, "No notes"),
		fmt.Sprintf("Hidden: %v", t.Hidden),
		"Parent: " + val(t.Parent, "Unknown"),
		fmt.Sprintf("Deleted: %v", t.Deleted),
		"Completed: " + ptrVal(t.Completed, "Unknown"),
		"Position: " + val(t.Position, "Unknown"),
		"ETag: " + val(t.Etag, "Unknown"),
		"Kind: " + val(t.Kind, "Unknown"),
		"Created: " + val(t.Updated, "Unknown"),
		"Updated: " + val(t.Updated, "Unknown"),
	}, "\n")
}
