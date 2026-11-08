# Integration Test Guide

This document outlines the steps to run a full integration test of the Nanosec system in a development environment.

## Prerequisites

- **MySQL Database**: Running at `app01.zwm.home:3306` (User: `nanosec`, Pass: `nanosec`)
- **LLM API**: Running at `http://devserver.zwm.home:3030/v1` (Model: `glm-4.7`)
- **Go 1.22+**: Installed locally.

## Configuration

We use a **directory-based configuration** strategy. The `configs/dev/` directory contains environment-specific settings.

### 1. Bus Config (`configs/dev/bus.json`)
Connects to the shared MySQL database and listens on WebSocket port 9000.

### 2. Gateway Config (`configs/dev/gateway.json`)
Points to the local Bus and enables the **CLI Channel** for user `admin`.

### 3. Agent Config (`configs/dev/agent.json`)
Configures the Agent to use the custom LLM endpoint (`devserver`) and defines the workspace directory.

## Execution Steps

Open three separate terminal windows.

### Terminal 1: The Bus (Infrastructure)
Start the Bus service first. It initializes the database connection and the WebSocket server.

```bash
go run ./cmd/nanobus --config configs/dev/bus.json
```
**Expected Output:**
```text
INFO bus started port=9000 database=mysql ...
```

### Terminal 2: The Agent (Brain)
Start the Agent for the user `admin`. It will poll the bus for tasks.

```bash
go run ./cmd/nanosec --user=admin --config configs/dev/agent.json
```
**Expected Output:**
```text
INFO agent started user=admin model=glm-4.7 ...
```

### Terminal 3: The Gateway (Interface)
Start the Gateway. This provides an interactive CLI to chat with the agent.

```bash
go run ./cmd/nanogateway --config configs/dev/gateway.json
```
**Expected Output:**
```text
CLI Gateway started for user 'admin'. Type your message (Ctrl+C to exit):
>
```

## Testing Scenarios

### 1. Basic Chat
**Input:** `Hello, who are you?`
**Expected Behavior:**
1. Gateway posts message to Bus.
2. Agent claims message, calls LLM.
3. LLM response is routed back to Gateway.
4. Gateway prints response (e.g., `🤖: I am Nanosec...`).

### 2. Tool Execution (Shell)
**Input:** `List the files in the current directory.`
**Expected Behavior:**
1. Agent receives request.
2. LLM generates a tool call (e.g., `ls -F`).
3. Agent executes the tool locally.
4. Agent sends tool output back to LLM.
5. LLM summarizes the result.
6. Gateway prints the final summary.

## Troubleshooting

- **Connection Refused**: Ensure Bus is running and port 9000 is accessible.
- **Database Error**: Check MySQL credentials in `configs/dev/bus.json` and ensuring network connectivity to `app01`.
- **LLM Error**: Check the API key and endpoint in `configs/dev/agent.json`. Ensure `devserver` is reachable.
