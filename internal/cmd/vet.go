package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime/trace"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/jackc/pgx/v5"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kyleconroy/sqlc/internal/config"
	"github.com/kyleconroy/sqlc/internal/debug"
	"github.com/kyleconroy/sqlc/internal/opts"
	"github.com/kyleconroy/sqlc/internal/plugin"
	"github.com/kyleconroy/sqlc/internal/shfmt"
	"github.com/kyleconroy/sqlc/internal/sql/ast"
)

var ErrFailedChecks = errors.New("failed checks")

var pjson = protojson.UnmarshalOptions{AllowPartial: true, DiscardUnknown: true}

const RuleDbPrepare = "sqlc/db-prepare"
const QueryFlagSqlcVetDisable = "@sqlc-vet-disable"

func NewCmdVet() *cobra.Command {
	return &cobra.Command{
		Use:   "vet",
		Short: "Vet examines queries",
		RunE: func(cmd *cobra.Command, args []string) error {
			defer trace.StartRegion(cmd.Context(), "vet").End()
			stderr := cmd.ErrOrStderr()
			dir, name := getConfigPath(stderr, cmd.Flag("file"))
			if err := Vet(cmd.Context(), ParseEnv(cmd), dir, name, stderr); err != nil {
				if !errors.Is(err, ErrFailedChecks) {
					fmt.Fprintf(stderr, "%s\n", err)
				}
				os.Exit(1)
			}
			return nil
		},
	}
}

func Vet(ctx context.Context, e Env, dir, filename string, stderr io.Writer) error {
	configPath, conf, err := readConfig(stderr, dir, filename)
	if err != nil {
		return err
	}

	base := filepath.Base(configPath)
	if err := config.Validate(conf); err != nil {
		fmt.Fprintf(stderr, "error validating %s: %s\n", base, err)
		return err
	}

	if err := e.Validate(conf); err != nil {
		fmt.Fprintf(stderr, "error validating %s: %s\n", base, err)
		return err
	}

	env, err := cel.NewEnv(
		cel.StdLib(),
		ext.Strings(ext.StringsVersion(1)),
		cel.Types(
			&plugin.VetConfig{},
			&plugin.VetQuery{},
			&plugin.PostgreSQLExplain{},
			&plugin.MySQLExplain{},
		),
		cel.Variable("query",
			cel.ObjectType("plugin.VetQuery"),
		),
		cel.Variable("config",
			cel.ObjectType("plugin.VetConfig"),
		),
		cel.Variable("postgresql",
			cel.ObjectType("plugin.PostgreSQL"),
		),
		cel.Variable("mysql",
			cel.ObjectType("plugin.MySQL"),
		),
	)
	if err != nil {
		return fmt.Errorf("new CEL env error: %s", err)
	}

	rules := map[string]rule{
		RuleDbPrepare: {NeedsPrepare: true},
	}

	for _, c := range conf.Rules {
		if c.Name == "" {
			return fmt.Errorf("rules require a name")
		}
		if _, found := rules[c.Name]; found {
			return fmt.Errorf("type-check error: a rule with the name '%s' already exists", c.Name)
		}
		if c.Rule == "" {
			return fmt.Errorf("type-check error: %s is empty", c.Name)
		}
		ast, issues := env.Compile(c.Rule)
		if issues != nil && issues.Err() != nil {
			return fmt.Errorf("type-check error: %s %s", c.Name, issues.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			return fmt.Errorf("program construction error: %s %s", c.Name, err)
		}
		rule := rule{Program: &prg, Message: c.Msg}

		// TODO There's probably a nicer way to do this from the ast
		// https://pkg.go.dev/github.com/google/cel-go/common/ast#AllMatcher
		if strings.Contains(c.Rule, "postgresql.explain") ||
			strings.Contains(c.Rule, "mysql.explain") {
			rule.NeedsExplain = true
		}

		rules[c.Name] = rule
	}

	c := checker{
		Rules:      rules,
		Conf:       conf,
		Dir:        dir,
		Env:        env,
		Envmap:     map[string]string{},
		Stderr:     stderr,
		NoDatabase: e.NoDatabase,
	}
	errored := false
	for _, sql := range conf.SQL {
		if err := c.checkSQL(ctx, sql); err != nil {
			if !errors.Is(err, ErrFailedChecks) {
				fmt.Fprintf(stderr, "%s\n", err)
			}
			errored = true
		}
	}
	if errored {
		return ErrFailedChecks
	}
	return nil
}

// Determine if a query can be prepared based on the engine and the statement
// type.
func prepareable(sql config.SQL, raw *ast.RawStmt) bool {
	if sql.Engine == config.EnginePostgreSQL {
		// TOOD: Add support for MERGE and VALUES stmts
		switch raw.Stmt.(type) {
		case *ast.DeleteStmt:
			return true
		case *ast.InsertStmt:
			return true
		case *ast.SelectStmt:
			return true
		case *ast.UpdateStmt:
			return true
		default:
			return false
		}
	}
	// Almost all statements in MySQL can be prepared, so I'm just going to assume they can be
	// https://dev.mysql.com/doc/refman/8.0/en/sql-prepared-statements.html
	if sql.Engine == config.EngineMySQL {
		return true
	}
	if sql.Engine == config.EngineSQLite {
		return true
	}
	return false
}

type preparer interface {
	Prepare(context.Context, string, string) error
}

type pgxConn struct {
	c *pgx.Conn
}

func (p *pgxConn) Prepare(ctx context.Context, name, query string) error {
	_, err := p.c.Prepare(ctx, name, query)
	return err
}

func (p *pgxConn) Explain(ctx context.Context, query string, args ...*plugin.Parameter) (*vetEngineOutput, error) {
	eQuery := "EXPLAIN (ANALYZE false, VERBOSE, COSTS, SETTINGS, BUFFERS, FORMAT JSON) "+query
	eArgs := make([]any, len(args))
	row := p.c.QueryRow(ctx, eQuery, eArgs...)
	var result []json.RawMessage
	if err := row.Scan(&result); err != nil {
		return nil, err
	}
	if debug.Debug.DumpExplain {
		fmt.Println(eQuery)
		fmt.Println(string(result[0]))
	}
	var explain plugin.PostgreSQLExplain
	if err := pjson.Unmarshal(result[0], &explain); err != nil {
		return nil, err
	}
	return &vetEngineOutput{PostgreSQL: &plugin.PostgreSQL{Explain: &explain}}, nil
}

type dbPreparer struct {
	db *sql.DB
}

func (p *dbPreparer) Prepare(ctx context.Context, name, query string) error {
	s, err := p.db.PrepareContext(ctx, query)
	s.Close()
	return err
}

type explainer interface {
	Explain(context.Context, string, ...*plugin.Parameter) (*vetEngineOutput, error)
}

type mysqlExplainer struct {
	*sql.DB
}

func (me *mysqlExplainer) Explain(ctx context.Context, query string, args ...*plugin.Parameter) (*vetEngineOutput, error) {
	eQuery := "EXPLAIN FORMAT=JSON "+query
	eArgs := make([]any, len(args))
	row := me.QueryRowContext(ctx, eQuery, eArgs...)
	var result json.RawMessage
	if err := row.Scan(&result); err != nil {
		return nil, err
	}
	if debug.Debug.DumpExplain {
		fmt.Println(eQuery)
		fmt.Println(string(result))
	}
	var explain plugin.MySQLExplain
	if err := pjson.Unmarshal(result, &explain); err != nil {
		return nil, err
	}
	if explain.QueryBlock.Message != "" {
		return nil, fmt.Errorf("mysql explain: %s", explain.QueryBlock.Message)
	}
	return &vetEngineOutput{MySQL: &plugin.MySQL{Explain: &explain}}, nil
}

type rule struct {
	Program      *cel.Program
	Message      string
	NeedsPrepare bool
	NeedsExplain bool
}

type checker struct {
	Rules      map[string]rule
	Conf       *config.Config
	Dir        string
	Env        *cel.Env
	Envmap     map[string]string
	Stderr     io.Writer
	NoDatabase bool
}

func (c *checker) DSN(dsn string) (string, error) {
	// Populate the environment variable map if it is empty
	if len(c.Envmap) == 0 {
		for _, e := range os.Environ() {
			k, v, _ := strings.Cut(e, "=")
			c.Envmap[k] = v
		}
	}
	return shfmt.Replace(dsn, c.Envmap), nil
}

func (c *checker) checkSQL(ctx context.Context, s config.SQL) error {
	// TODO: Create a separate function for this logic so we can
	combo := config.Combine(*c.Conf, s)

	// TODO: This feels like a hack that will bite us later
	joined := make([]string, 0, len(s.Schema))
	for _, s := range s.Schema {
		joined = append(joined, filepath.Join(c.Dir, s))
	}
	s.Schema = joined

	joined = make([]string, 0, len(s.Queries))
	for _, q := range s.Queries {
		joined = append(joined, filepath.Join(c.Dir, q))
	}
	s.Queries = joined

	var name string
	parseOpts := opts.Parser{
		Debug: debug.Debug,
	}

	result, failed := parse(ctx, name, c.Dir, s, combo, parseOpts, c.Stderr)
	if failed {
		return ErrFailedChecks
	}

	var prep preparer
	var expl explainer
	if s.Database != nil { // TODO only set up a database connection if a rule evaluation requires it
		if c.NoDatabase {
			return fmt.Errorf("database: connections disabled via command line flag")
		}
		dburl, err := c.DSN(s.Database.URI)
		if err != nil {
			return err
		}
		switch s.Engine {
		case config.EnginePostgreSQL:
			conn, err := pgx.Connect(ctx, dburl)
			if err != nil {
				return fmt.Errorf("database: connection error: %s", err)
			}
			if err := conn.Ping(ctx); err != nil {
				return fmt.Errorf("database: connection error: %s", err)
			}
			defer conn.Close(ctx)
			pConn := &pgxConn{conn}
			prep = pConn
			expl = pConn
		case config.EngineMySQL:
			db, err := sql.Open("mysql", dburl)
			if err != nil {
				return fmt.Errorf("database: connection error: %s", err)
			}
			if err := db.PingContext(ctx); err != nil {
				return fmt.Errorf("database: connection error: %s", err)
			}
			defer db.Close()
			prep = &dbPreparer{db}
			expl = &mysqlExplainer{db}
		case config.EngineSQLite:
			db, err := sql.Open("sqlite3", dburl)
			if err != nil {
				return fmt.Errorf("database: connection error: %s", err)
			}
			if err := db.PingContext(ctx); err != nil {
				return fmt.Errorf("database: connection error: %s", err)
			}
			defer db.Close()
			prep = &dbPreparer{db}
			// SQLite really doesn't want us to depend on the output of EXPLAIN
			// QUERY PLAN: https://www.sqlite.org/eqp.html
			expl = nil
		default:
			return fmt.Errorf("unsupported database uri: %s", s.Engine)
		}
	}

	errored := false
	req := codeGenRequest(result, combo)
	cfg := vetConfig(req)
	for i, query := range req.Queries {
		if result.Queries[i].Flags[QueryFlagSqlcVetDisable] {
			if debug.Active {
				log.Printf("Skipping vet rules for query: %s\n", query.Name)
			}
			continue
		}

		evalMap := map[string]any{
			"query":  vetQuery(query),
			"config": cfg,
		}

		for _, name := range s.Rules {
			rule, ok := c.Rules[name]
			if !ok {
				return fmt.Errorf("type-check error: a rule with the name '%s' does not exist", name)
			}

			if rule.NeedsPrepare {
				if prep == nil {
					fmt.Fprintf(c.Stderr, "%s: %s: %s: error preparing query: database connection required\n", query.Filename, query.Name, name)
					errored = true
					continue
				}
				if !prepareable(s, result.Queries[i].RawStmt) {
					fmt.Fprintf(c.Stderr, "%s: %s: %s: error preparing query: %s\n", query.Filename, query.Name, name, "query type is unpreparable")
					errored = true
					continue
				}
				name := fmt.Sprintf("sqlc_vet_%d_%d", time.Now().Unix(), i)
				if err := prep.Prepare(ctx, name, query.Text); err != nil {
					fmt.Fprintf(c.Stderr, "%s: %s: %s: error preparing query: %s\n", query.Filename, query.Name, name, err)
					errored = true
					continue
				}
			}

			// short-circuit for "sqlc/db-prepare" rule which doesn't have a CEL program
			if rule.Program == nil {
				continue
			}

			// Get explain output for this query if we need it
			_, pgsqlOK := evalMap["postgresql"]; _, mysqlOK := evalMap["mysql"]
			if rule.NeedsExplain && !(pgsqlOK || mysqlOK) {
				if expl == nil {
					fmt.Fprintf(c.Stderr, "%s: %s: %s: error explaining query: database connection required\n", query.Filename, query.Name, name)
					errored = true
					continue
				}
				engineOutput, err := expl.Explain(ctx, query.Text, query.Params...)
				if err != nil {
					fmt.Fprintf(c.Stderr, "%s: %s: %s: error explaining query: %s\n", query.Filename, query.Name, name, err)
					errored = true
					continue
				}

				evalMap["postgresql"] = engineOutput.PostgreSQL
				evalMap["mysql"] = engineOutput.MySQL
			}

			out, _, err := (*rule.Program).Eval(evalMap)
			if err != nil {
				return err
			}
			tripped, ok := out.Value().(bool)
			if !ok {
				return fmt.Errorf("expression returned non-bool value: %v", out.Value())
			}
			if tripped {
				// TODO: Get line numbers in the output
				if rule.Message == "" {
					fmt.Fprintf(c.Stderr, "%s: %s: %s\n", query.Filename, query.Name, name)
				} else {
					fmt.Fprintf(c.Stderr, "%s: %s: %s: %s\n", query.Filename, query.Name, name, rule.Message)
				}
				errored = true
			}
		}
	}
	if errored {
		return ErrFailedChecks
	}
	return nil
}

func vetConfig(req *plugin.CodeGenRequest) *plugin.VetConfig {
	return &plugin.VetConfig{
		Version: req.Settings.Version,
		Engine:  req.Settings.Engine,
		Schema:  req.Settings.Schema,
		Queries: req.Settings.Queries,
	}
}

func vetQuery(q *plugin.Query) *plugin.VetQuery {
	var params []*plugin.VetParameter
	for _, p := range q.Params {
		params = append(params, &plugin.VetParameter{
			Number: p.Number,
		})
	}
	return &plugin.VetQuery{
		Sql:    q.Text,
		Name:   q.Name,
		Cmd:    strings.TrimPrefix(":", q.Cmd),
		Params: params,
	}
}

type vetEngineOutput struct {
	PostgreSQL *plugin.PostgreSQL
	MySQL      *plugin.MySQL
}
