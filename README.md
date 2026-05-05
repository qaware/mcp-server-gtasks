# MCP Server for Google Tasks

A reference implementation of an [MCP](https://modelcontextprotocol.io/) server for
[Google Tasks](https://developers.google.com/tasks), built with
the [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk).

The server itself does **not** perform any authentication. It expects an upstream
gateway to authenticate the user and forward a Google OAuth access token (with the
Google Tasks scope) in the standard `Authorization: Bearer <token>` header. The token
is passed straight through to the Google Tasks API.

## Features

### Tools

| Tool     | Description                            |
|----------|----------------------------------------|
| `search` | Search for tasks by title or notes     |
| `list`   | List all tasks across all task lists   |
| `create` | Create a new task                      |
| `update` | Update an existing task                |
| `delete` | Delete a task                          |
| `clear`  | Clear completed tasks from a task list |

### Resources

| URI Pattern          | Description                    |
|----------------------|--------------------------------|
| `gtasks:///{taskId}` | Read detailed task information |

## Architecture

```
MCP Client ──► Auth Gateway ──► MCP Server ──► Google Tasks API
                  (issues          (forwards
                   Google           Bearer
                   access           token)
                   token)
```

The gateway is responsible for the entire OAuth flow, token storage, and refresh.
This server only:

1. Reads the `Authorization: Bearer <token>` header.
2. Returns `401 Unauthorized` if the header is missing.
3. Uses the token as a Google OAuth access token when calling the Tasks API.

If Google rejects the token (expired, wrong scope, etc.), the corresponding tool call
fails with the error returned by Google.

### Endpoints

| Endpoint | Method | Description                                        |
|----------|--------|----------------------------------------------------|
| `/mcp`   | POST   | MCP Streamable HTTP (requires Bearer access token) |

## Configuration

| Variable | Required | Default | Description      |
|----------|----------|---------|------------------|
| `PORT`   | No       | `8080`  | HTTP server port |

## Run

```bash
go run .
```

## Docker

```bash
docker run --rm -p 8080:8080 \
  ghcr.io/agentic-layer/mcp-server-gtasks:latest
```

## MCP Client Configuration

```json
{
  "mcpServers": {
    "gtasks": {
      "url": "http://localhost:8080/mcp",
      "headers": {
        "Authorization": "Bearer <google-access-token>"
      }
    }
  }
}
```

In production the gateway injects the `Authorization` header on behalf of the client.

## Getting a Temporary Token for Testing

The token must be a Google OAuth access token with the
`https://www.googleapis.com/auth/tasks` scope. Pick whichever option is most convenient.

### Option 1: Google OAuth 2.0 Playground (no setup)

1. Open <https://developers.google.com/oauthplayground/>.
2. In **Step 1**, find **Tasks API v1** and select
   `https://www.googleapis.com/auth/tasks`. Click **Authorize APIs** and complete the
   Google sign-in.
3. In **Step 2**, click **Exchange authorization code for tokens**.
4. Copy the **Access token** value. It is valid for ~1 hour.

### Option 2: `gcloud` CLI

Requires a Google Cloud SDK install and a Google account.

```bash
gcloud auth application-default login \
  --scopes=https://www.googleapis.com/auth/tasks,openid

gcloud auth application-default print-access-token
```

The printed token is a short-lived Google access token you can pass to the server.

### Smoke test

```bash
TOKEN="<paste-token-here>"

# Verify the token has the Tasks scope.
curl -s "https://www.googleapis.com/oauth2/v3/tokeninfo?access_token=$TOKEN"

# Hit the MCP endpoint.
curl -i -X POST http://localhost:8080/mcp \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}'
```
