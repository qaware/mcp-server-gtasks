package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/api/tasks/v1"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()

	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		token := extractBearerToken(r)
		if token == "" {
			return nil
		}

		svc, err := newTasksService(r.Context(), token)
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

	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if extractBearerToken(r) == "" {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "Unauthorized: missing Bearer token", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	})

	log.Printf("MCP server listening on :%s", port)
	log.Printf("  MCP endpoint: http://localhost:%s/mcp", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}

func newTasksService(ctx context.Context, accessToken string) (*tasks.Service, error) {
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken})
	return tasks.NewService(ctx, option.WithTokenSource(tokenSource))
}

func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
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
