# merge-with-label
[![Actions Status](https://github.com/Eun/merge-with-label/workflows/push/badge.svg)](https://github.com/Eun/merge-with-label/actions)
[![Coverage Status](https://coveralls.io/repos/github/Eun/merge-with-label/badge.svg?branch=master)](https://coveralls.io/github/Eun/merge-with-label?branch=master)
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
  #  - "dont-update"
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
   | Permission    | Level          |
   |---------------|----------------|
   | Checks        | Read-Only      |
   | Contents      | Read and write |
   | Metadata      | Read-Only      |
   | Pull requests | Read and write |
   | Workflows     | Read and write |

   ### Subscribe to events 
   - Check run
   - Pull request
   - Pull request review
   - Push
2. Create a private key and save it
3. Note down the app id
4. Spin up the instance somewhere using `docker compose`
   ### docker-compose.yml
   ```yaml
   version: '3.9'
   services:
     nats:
       image: nats:2.9.20
       command: ["--js", "-user", "nats", "-pass", "425751fd-62e2-4b73-9e1b-5a9b0dafc5ad"]
       ports:
         - "4222:4222"
   
     server:
       image: ghcr.io/eun/merge-with-label:latest
       command: "server"
       ports:
         - "8000:8000"
       environment:
         PORT: 8000
         NATS_URL: nats://nats:425751fd-62e2-4b73-9e1b-5a9b0dafc5ad@nats:4222
       depends_on:
         - nats
   
     worker:
       image: ghcr.io/eun/merge-with-label:latest
       command: "worker"
       volumes:
         - "./private-key.pem:/private-key.pem:ro"
       environment:
         NATS_URL: nats://nats:425751fd-62e2-4b73-9e1b-5a9b0dafc5ad@nats:4222
         APP_ID: <your app id>
         PRIVATE_KEY: /private-key.pem
       depends_on:
         - server
   ```
   > Make sure you fill in your app id, provide the private-key.pem file
   > and modify the nats username and password
5. Point the webhook url to the deployment


### Fine Tuning Settings
Following environment variables are available

| Variable                          | Default Value       |
|-----------------------------------|---------------------|
| `AllowedRepositories`             | `.*`                |
| `BotName`                         | `merge-with-label`  |
| `StreamName`                      | `mwl_bot_events`    |
| `PullRequestSubject`              | `pull_request`      |
| `PushSubject`                     | `push`              |
| `MessageRetryAttempts`            | `5`                 |
| `MessageRetryWait`                | `15s`               |
| `RateLimitBucketName`             | `mwl_rate_limit`    |
| `RateLimitBucketTTL`              | `24h`               |
| `RateLimitInterval`               | `30s`               |
| `AccessTokensBucketName`          | `mwl_access_tokens` |
| `AccessTokensBucketTTL`           | `24h`               |
| `ConfigsBucketName`               | `mwl_configs`       |
| `ConfigsBucketTTL`                | `24h`               |
| `CheckRunsBucketName`             | `mwl_check_runs`    |
| `CheckRunsBucketTTL`              | `10m`               |
| `DurationBeforeMergeAfterCheck`   | `10s`               |
| `DurationToWaitAfterUpdateBranch` | `30s`               |
| `MaxMessageAge`                   | `10m`               |
| `MessageChannelSizePerSubject`    | `64`                |

> Additionally, you can enable debug logging by setting the `DEBUG`
> environment variable to `true`.

## Build History
[![Build history](https://buildstats.info/github/chart/Eun/merge-with-label?branch=master)](https://github.com/Eun/merge-with-label/actions)
