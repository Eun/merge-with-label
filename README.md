<p align="center">
  <img width="829" height="170" src="header.png">
</p>

[![Actions Status](https://github.com/Eun/merge-with-label/workflows/push/badge.svg)](https://github.com/Eun/merge-with-label/actions)
[![Coverage Status](https://coveralls.io/repos/github/Eun/merge-with-label/badge.svg?branch=main)](https://coveralls.io/github/Eun/merge-with-label?branch=main)
[![PkgGoDev](https://img.shields.io/badge/pkg.go.dev-reference-blue)](https://pkg.go.dev/github.com/Eun/merge-with-label)
[![go-report](https://goreportcard.com/badge/github.com/Eun/merge-with-label)](https://goreportcard.com/report/github.com/Eun/merge-with-label)
---
A github bot for merging & updating pull requests with a label.

## Functionality
This bot can merge and keep your branches up to date with the latest changes from base (master/main).

## Config file
Place `merge-with-label.yml` in `.github` repository:

```yaml
version: 1
merge:
  # specify a list of labels that indicate whether a pull request is eligible
  # for merging (regex)
  # (or-list, only one label must be present on a pull request)
  # (leave empty to disable the merge feature)
  labels:
    - "merge"
  # strategy to merge (can be "commit", "squash" or "rebase")
  strategy: "squash"
  # amount of required approvals before merging
  #requiredApprovals: 1
  # specify a list of users that are required for review (regex)
  # (and-list, all users need to approve)
  #requireApprovalsFrom:
  #  -
  # names of the checks that are need to pass before merging (regex)
  # (and-list, all checks need to pass)
  requiredChecks:
    - ".*"
  # require a linear history
  requireLinearHistory: false
  # delete branch after merging
  deleteBranch: true
  # never merge pull requests that were created by these users (regex)
  #ignoreFromUsers:
  #  - "dependabot"
  # never merge pull requests that match one of these titles (regex)
  #ignoreWithTitles:
  #  - "chore:.+"
  # never update pull requests that match one of these labels (regex)
  #ignoreWithLabels:
  #  - "dont-merge"
update:
  # specify a list of labels that indicate whether a pull request is eligible
  # for updating (regex)
  # (or-list, only one label must be present on a pull request)
  # (leave empty to disable the update feature)
  labels: 
    - "update-branch"
  # never update pull requests that were created by these users (regex)
  ignoreFromUsers:
    - "dependabot"
  # never update pull requests that match one of these titles (regex)
  #ignoreWithTitles:
  #  - "chore:.+"
  # never update pull requests that match one of these titles (regex)
  #ignoreWithTitles:
  #  - "chore:.+"
  # never update pull requests that match one of these labels (regex)
  #ignoreWithLabels:
  #  - "dont-update"
```

## Setup
1. Create a new github app with following permissions & events
   ### Repository Permissions
   | Permission      | Level          |
   |-----------------|----------------|
   | Actions         | Read           |
   | Checks          | Read and write |
   | Commit statuses | Read-Only      |
   | Contents        | Read and write |
   | Metadata        | Read-Only      |
   | Pull requests   | Read and write |
   | Workflows       | Read and write |

   ### Subscribe to events 
   - Check run
   - Pull request
   - Pull request review
   - Push
   - Status
2. Create a private key and save it
3. Note down the app id
4. Spin up the instance somewhere using `docker compose`
   ### docker-compose.yml
   ```yaml
   version: '3.9'
   services:
     postgres:
       image: supabase/postgres:17.6.1.151
       restart: unless-stopped
       command:
         - postgres
         - -c
         - cron.database_name=merge_with_label
       volumes:
         - ./pg_data:/var/lib/postgresql/data
       environment:
         POSTGRES_USER: mwl
         POSTGRES_PASSWORD: <your postgres password>
         POSTGRES_DB: merge_with_label
       healthcheck:
         test: ["CMD-SHELL", "pg_isready -U mwl -d merge_with_label"]
         interval: 5s
         timeout: 5s
         retries: 10
   
     server:
       image: ghcr.io/eun/merge-with-label-server:latest
       restart: unless-stopped
       ports:
         - "8000:8000"
       environment:
         PORT: 8000
         PostgresDSN: "postgres://mwl:<your postgres password>@postgres:5432/merge_with_label?sslmode=disable"
       depends_on:
         postgres:
           condition: service_healthy
       healthcheck:
         test: ["CMD-SHELL", "wget -qO- http://localhost:8000/ || exit 1"]
         interval: 5s
         timeout: 5s
         retries: 12
   
     worker:
       image: ghcr.io/eun/merge-with-label-worker:latest
       restart: unless-stopped
       volumes:
         - "./private-key.pem:/private-key.pem:ro"
       environment:
         PostgresDSN: "postgres://mwl:<your postgres password>@postgres:5432/merge_with_label?sslmode=disable"
         APP_ID: <your app id>
         PRIVATE_KEY: /private-key.pem
       depends_on:
         postgres:
           condition: service_healthy
         server:
           condition: service_healthy
       deploy:
         replicas: 1
   ```
   > Make sure you fill in your app id, provide the private-key.pem file
   > and set a secure Postgres password
5. Point the webhook url to the deployment


### Fine Tuning Settings
Following environment variables are available

#### Server & Worker

| Variable                      | Default Value      | Description                                      |
|-------------------------------|--------------------|--------------------------------------------------|
| `PostgresDSN`                 | *(required)*       | PostgreSQL connection string                     |
| `AllowedRepositories`         | `.*`               | Regex of repositories the bot may act on         |
| `AllowOnlyPublicRepositories` | `false`            | Ignore events from private repositories          |
| `RateLimitInterval`           | `30s`              | Minimum interval between merges for the same PR  |
| `DEBUG`                       | `false`            | Enable debug logging                             |
| `TRACE`                       | `false`            | Enable trace-level logging                       |

#### Server only

| Variable  | Default Value | Description                                            |
|-----------|---------------|--------------------------------------------------------|
| `ADDRESS` | *(unset)*     | Full listen address (e.g. `0.0.0.0:8000`); overrides `PORT` |
| `PORT`    | `8000`        | Port to listen on (used when `ADDRESS` is not set)     |

#### Worker only

| Variable                          | Default Value      | Description                                          |
|-----------------------------------|--------------------|------------------------------------------------------|
| `APP_ID`                          | *(required)*       | GitHub App ID                                        |
| `PRIVATE_KEY`                     | *(required)*       | Path to the GitHub App private key PEM file          |
| `BotName`                         | `merge-with-label` | GitHub App bot username                              |
| `MessageRetryAttempts`            | `5`                | Number of times to retry a failed job                |
| `MessageRetryWait`                | `15s`              | Wait duration between retries                        |
| `MaxConcurrentJobs`               | `10`               | Maximum number of parallel worker jobs               |
| `DurationBeforeMergeAfterCheck`   | `10s`              | Wait after a check passes before merging             |
| `DurationToWaitAfterUpdateBranch` | `30s`              | Wait after updating a branch before re-checking      |
| `MessageChannelSizePerSubject`    | `64`               | Internal channel buffer size per queue subject       |

## Build History
[![Build history](https://buildstats.info/github/chart/Eun/merge-with-label?branch=master)](https://github.com/Eun/merge-with-label/actions)
