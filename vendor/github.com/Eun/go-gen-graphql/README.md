# go-gen-graphql
[![Actions Status](https://github.com/Eun/go-gen-graphql/workflows/push/badge.svg)](https://github.com/Eun/go-gen-graphql/actions)
[![Coverage Status](https://coveralls.io/repos/github/Eun/go-gen-graphql/badge.svg?branch=master)](https://coveralls.io/github/Eun/go-gen-graphql?branch=master)
[![PkgGoDev](https://img.shields.io/badge/pkg.go.dev-reference-blue)](https://pkg.go.dev/github.com/Eun/go-gen-graphql)
[![go-report](https://goreportcard.com/badge/github.com/Eun/go-gen-graphql)](https://goreportcard.com/report/github.com/Eun/go-gen-graphql)
---
Generate a graphql query/mutation body from a struct

```go
package main

import (
	"fmt"
	gengraphql "github.com/Eun/go-gen-graphql"
)

func main() {
	type Data struct {
		ID             string
		Name           string `json:"name"`
		CreationTime   string `graphql:"createdOn"`
		ActiveProjects struct {
			ID string `json:"id"`
		} `json:"projects" graphql:"projects(filter: %q)"`
	}

	s, err := gengraphql.Generatef(Data{}, nil, "active")
	if err != nil {
		panic(err)
	}
	fmt.Println(s)
	// Output:
	// ID
	// name
	// createdOn
	// projects(filter: "active"){
	//   id
	// }
}
```

## Build History
[![Build history](https://buildstats.info/github/chart/Eun/go-gen-graphql?branch=master)](https://github.com/Eun/go-gen-graphql/actions)
