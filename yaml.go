package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"regexp"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/checker/decls"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/interpreter/functions"
	"github.com/google/go-github/github"
	exprpb "google.golang.org/genproto/googleapis/api/expr/v1alpha1"
	"gopkg.in/yaml.v2"
)

const (
	defaultExpr = `ce.type == "com.example.someevent"`
	defaultCE   = `
		{
			"specversion" : "0.2",
			"type" : "com.example.someevent",
			"owner": "tektoncd",
			"repo": "pipeline",
			"ref": "refs/heads/master",
			"source" : "/mycontext",
			"id" : "A234-1234-1234",
			"time" : "2018-04-05T17:31:00Z",
			"comexampleextension1" : "value",
			"comexampleextension2" : {
					"otherValue": 5,
					"stringValue": "value"
			},
			"contenttype" : "text/xml",
			"data" : "<much wow=\"xml\"/>"
		}`
)

var (
	expr   = flag.String("e", defaultExpr, "expression to evaluate")
	ceJSON = flag.String("ce", defaultCE, "CloudEvent as JSON")
)

func dynamicValue(i interface{}) ref.Val {
	// Terrible hack to get dynamic values. Consider making the API calls
	// directly instead.
	b, err := json.Marshal(i)
	if err != nil {
		return types.NewErr(err.Error())
	}
	var s map[string]interface{}
	if err := json.Unmarshal(b, &s); err != nil {
		return types.NewErr(err.Error())
	}
	return types.DefaultTypeAdapter.NativeToValue(s)
}

func main() {
	flag.Parse()
	ctx := context.Background()

	var cloudEvent map[string]interface{}
	if err := json.Unmarshal([]byte(*ceJSON), &cloudEvent); err != nil {
		log.Fatalf("json parse error: %s\n", err)
	}

	gh := github.NewClient(nil)
	funcs := cel.Functions(
		&functions.Overload{
			Operator: "commit",
			Function: func(values ...ref.Val) ref.Val {
				if len(values) != 3 {
					return types.NewErr("invalid args")
				}
				owner := values[0].Value().(string)
				repo := values[1].Value().(string)
				rev := values[2].Value().(string)
				c, _, err := gh.Repositories.GetCommit(ctx, owner, repo, rev)
				if err != nil {
					return types.NewErr(err.Error())
				}
				return dynamicValue(c)
			},
		},
		&functions.Overload{
			Operator: "pr",
			Function: func(values ...ref.Val) ref.Val {
				if len(values) != 3 {
					return types.NewErr("invalid args")
				}
				owner := values[0].Value().(string)
				repo := values[1].Value().(string)
				pr := values[2].Value().(int64)
				c, _, err := gh.PullRequests.Get(ctx, owner, repo, int(pr))
				if err != nil {
					return types.NewErr(err.Error())
				}
				return dynamicValue(c)
			},
		},
		&functions.Overload{
			Operator: "collaborator",
			Function: func(values ...ref.Val) ref.Val {
				if len(values) != 3 {
					return types.NewErr("invalid args")
				}
				owner := values[0].Value().(string)
				repo := values[1].Value().(string)
				user := values[2].Value().(string)
				c, _, err := gh.Repositories.IsCollaborator(ctx, owner, repo, user)
				if err != nil {
					return types.NewErr(err.Error())
				}
				return types.Bool(c)
			},
		},
	)

	// Create the CEL environment with declarations for the input attributes and
	// the desired extension functions. In many cases the desired functionality will
	// be present in a built-in function.
	e, err := cel.NewEnv(
		cel.Declarations(
			decls.NewIdent("ce", decls.Dyn, nil),
			decls.NewFunction("commit",
				decls.NewOverload("commit",
					[]*exprpb.Type{decls.String, decls.String, decls.String},
					decls.Dyn,
				),
			),
			decls.NewFunction("pr",
				decls.NewOverload("pr",
					[]*exprpb.Type{decls.String, decls.String, decls.Int},
					decls.Dyn,
				),
			),
			decls.NewFunction("collaborator",
				decls.NewOverload("collaborator",
					[]*exprpb.Type{decls.String, decls.String, decls.String},
					decls.Bool,
				),
			),
			decls.NewIdent("cat", decls.String, nil),
		),
	)
	if err != nil {
		log.Fatalf("environment creation error: %s\n", err)
	}

	d := map[interface{}]interface{}{}
	if err := yaml.Unmarshal([]byte(*expr), &d); err != nil {
		log.Fatalln(err)
	}
	log.Println("in: ", d)

	for k, _ := range d {
		s, ok := d[k].(string)
		if !ok {
			continue
		}
		if ok, err := regexp.Match(`\$\(.*\)`, []byte(s)); !ok || err != nil {
			continue
		}
		s = strings.TrimPrefix(s, "$(")
		s = strings.TrimSuffix(s, ")")
		// Parse and check the expression.
		p, iss := e.Parse(s)
		if iss != nil && iss.Err() != nil {
			log.Fatalln(iss.Err())
		}
		c, iss := e.Check(p)
		if iss != nil && iss.Err() != nil {
			log.Fatalln(iss.Err())
		}
		prg, err := e.Program(c, funcs)
		if err != nil {
			log.Fatalf("program creation error: %s\n", err)
		}

		// Evaluate the program against some inputs. Note: the details return is not used.
		out, _, err := prg.Eval(map[string]interface{}{
			// Native values are converted to CEL values under the covers.
			"ce":  cloudEvent,
			"cat": "🐱",
		})
		if err != nil {
			log.Fatalf("runtime error: %s\n", err)
		}

		d[k] = out.Value()
	}
	log.Println("out: ", d)
}