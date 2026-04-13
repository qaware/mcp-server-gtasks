# MCP Server for Google Tasks

A reference implementation of an [MCP](https://modelcontextprotocol.io/) server for
[Google Tasks](https://developers.google.com/tasks), built with
the [Go MCP SDK](https://github.com/modelcontextprotocol/go-sdk).

This server showcases **OAuth 2.1 authentication** with Google in an MCP server context.
It is not designed to be production-ready, but serves as a reference implementation.

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

The server implements the [MCP Authorization specification](https://modelcontextprotocol.io/specification/2025-03-26/basic/authorization)
with OAuth 2.1 and acts as a **third-party authorization server** that delegates to Google OAuth.

Users do not need their own Google Cloud OAuth credentials — the server provides its own.

### OAuth 2.1 Flow

```
MCP Client                    MCP Server                    Google
    |                              |                           |
    |-- GET /mcp ----------------->|                           |
    |<-- 401 + WWW-Authenticate ---|                           |
    |                              |                           |
    |-- GET /.well-known/oauth---->|                           |
    |   authorization-server       |                           |
    |<-- metadata (endpoints) -----|                           |
    |                              |                           |
    |-- POST /register ----------->|                           |
    |<-- client_id ----------------|                           |
    |                              |                           |
    |-- GET /authorize ----------->|                           |
    |   (PKCE code_challenge)      |-- redirect to Google ---->|
    |                              |                           |
    |                              |<-- auth code -------------|
    |<-- redirect with our code ---|                           |
    |                              |                           |
    |-- POST /token -------------->|                           |
    |   (code + code_verifier)     |                           |
    |<-- access_token -------------|                           |
    |                              |                           |
    |-- GET /mcp ----------------->|                           |
    |   (Bearer access_token)      |-- Google Tasks API ------>|
    |<-- MCP response -------------|<-- task data -------------|
```

### Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/.well-known/oauth-authorization-server` | GET | RFC 8414 Authorization Server Metadata |
| `/.well-known/oauth-protected-resource` | GET | RFC 9728 Protected Resource Metadata |
| `/register` | POST | RFC 7591 Dynamic Client Registration |
| `/authorize` | GET | OAuth 2.1 Authorization (redirects to Google) |
| `/google/callback` | GET | Google OAuth callback (internal) |
| `/token` | POST | Token exchange and refresh |
| `/mcp` | POST | MCP Streamable HTTP (requires Bearer token) |

## Prerequisites

1. A Google Cloud project with the [Google Tasks API](https://console.cloud.google.com/apis/library/tasks.googleapis.com) enabled
2. OAuth2 credentials (**Web application** type) from [Google Cloud Console](https://console.cloud.google.com/apis/credentials)
3. Add `{BASE_URL}/google/callback` as an **Authorized redirect URI**

## Configuration

All configuration is via environment variables:

| Variable               | Required | Default                  | Description                     |
|------------------------|----------|--------------------------|---------------------------------|
| `GOOGLE_CLIENT_ID`     | Yes      |                          | OAuth2 client ID                |
| `GOOGLE_CLIENT_SECRET` | Yes      |                          | OAuth2 client secret            |
| `PORT`                 | No       | `8080`                   | HTTP server port                |
| `BASE_URL`             | No       | `http://localhost:$PORT` | Public base URL (for redirects) |

## Run

```bash
export GOOGLE_CLIENT_ID="your-client-id"
export GOOGLE_CLIENT_SECRET="your-client-secret"

go run .
```

## Docker

```bash
docker run --rm -p 8080:8080 \
  -e GOOGLE_CLIENT_ID="your-client-id" \
  -e GOOGLE_CLIENT_SECRET="your-client-secret" \
  ghcr.io/agentic-layer/mcp-server-gtasks:latest
```

## MCP Client Configuration

### Claude Desktop

```json
{
  "mcpServers": {
    "gtasks": {
      "url": "http://localhost:8080/mcp"
    }
  }
}
```

Authentication is handled automatically via the OAuth 2.1 flow — the MCP client
will discover the authorization server metadata and guide the user through login.
