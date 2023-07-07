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
  # name of the label (leave empty to disable the merge feature)
  label: "merge"
  # strategy to merge (can be "commit", "squash" or "rebase")
  strategy: "squash"
  # amount of required approvals before merging
  requiredApprovals: 1
  # names of the checks that are need to pass before merging
  requiredChecks:
    - "linter"
    - "test"
  # never merge pull requests that were created by these users
  #ignoreFromUsers:
  #  - "dependabot"
  # never merge pull requests that match one of these titles (regex)
  #ignoreWithTitles:
  #  - "chore:.+"
update:
  # name of the label  (leave empty to disable the update feature)
  label: "update-branch"
  ignoreFromUsers:
    - "dependabot"
  # never merge pull requests that match one of these titles (regex)
  #ignoreWithTitles:
  #  - "chore:.+"
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
   - Push
2. Create a private key and save it
3. Note down the app id
4. Spin up the instance somewhere using `docker compose`
   ### docker-compose.yml
   ```yaml
   version: '3.9'
   services:
     redis:
       image: redis
       ports:
         - "6379:6379"
   
     service:
       image: ghcr.io/eun/merge-with-label:latest
       ports:
         - "8000:8000"
       volumes:
         - "./private-key.pem:/private-key.pem:ro"
       environment:
         PORT: 8000
         REDIS_HOST: redis:6379
         APP_ID: <your app id>
         PRIVATE_KEY: /private-key.pem
       depends_on:
         - redis
   ```
   > Make sure you fill in your app id and provide the private-key.pem file
5. Point the webhook url to the deployment


## Build History
[![Build history](https://buildstats.info/github/chart/Eun/merge-with-label?branch=master)](https://github.com/Eun/merge-with-label/actions)
