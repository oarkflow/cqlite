# sqlparser — High-Performance SQL Parser for Go

A **zero-allocation hot-path** SQL parser written in Go, designed for high throughput, low latency, and production use over a documented MySQL/PostgreSQL-oriented SQL subset.

The parser has two modes:
- **Fast AST mode** (`ParseStatement`, `ParseStatements`, reusable `Parser`) skips trivia and is optimized for warmed zero-allocation parsing.
- **Lossless document mode** (`ParseDocument`) preserves whitespace, comments, tokens, and statement source spans for exact round-tripping.

---

## Performance Design

| Technique | Benefit |
|---|---|
| **Hand-rolled lexer** | No regex, no reflection — pure byte-scan state machine |
| **Char-class dispatch table** | `charClass[256]` table replaces branch-heavy switch for token dispatch |
| **Two-level keyword lookup** | `[len][firstChar]` bucketed lookup — avg 1 comparison per lookup |
| **No line/col tracking on hot path** | Position computed lazily only on error via `ComputeLineCol()` |
| **Compact Token struct** | `TokenType` is `uint8`, `Pos` is `int32`, no Line/Col — 32 bytes total |
| **Unsafe string→[]byte** | `NewString()` avoids source copy via `unsafe.StringData` |
| **Pratt expression parser** | Single-pass, no backtracking, minimal stack depth |
| **2-token lookahead** | No token buffer growth; decisions made with peek only |
| **Arena allocator** | All AST nodes come from a reusable slab → zero GC pressure on warm path |
| **Byte-slice AST** | `Token.Raw` is a sub-slice of source — no string copies |

### Benchmark Results (i9-13900K, Go 1.26)

```
BenchmarkParseSimpleSelect-32     ~293 ns/op   133 MB/s    0 allocs/op   (arena warm)
BenchmarkParseInsert-32           ~269 ns/op   283 MB/s    0 allocs/op   (arena warm)
BenchmarkParseCreateTable-32      ~1.2 µs/op   312 MB/s    0 allocs/op   (arena warm)
BenchmarkParseSelect-32           ~1.9 µs/op   219 MB/s    0 allocs/op   (arena warm, complex 16-line query)
BenchmarkTokenize-32              ~1.2 µs/op   361 MB/s    0 allocs/op
```

Simple SELECT/INSERT queries parse in **under 300ns** with zero allocations on a warm arena.

---

## Production Support Contract

This project is production-ready for the documented parser surface below. It is not a full semantic clone of every database engine.

| Level | Meaning |
|---|---|
| Fully modeled | Parsed into dedicated AST nodes and rendered/analyzed by public APIs. |
| Structured/raw fallback | Accepted and source-preserved, but internals may be dialect-specific and not fully analyzed. |
| Unsupported | Returns a parse error unless represented by a generic fallback. |

Use `ParseOptions` for untrusted SQL: set `MaxBytes`, `MaxTokens`, `MaxDepth`, `MaxStatements`, and set `AllowGenericDDL` according to your policy. Use `ConvertOptions{Strict:true}` when conversion must fail instead of rendering best-effort fallback SQL.

## SQL Coverage

### DML
- `SELECT` — columns, aliases, `*`, qualified names
- `FROM` — simple tables, subqueries, aliases
- `JOIN` — INNER, LEFT, RIGHT, FULL, CROSS, NATURAL with ON / USING
- `WHERE`, `GROUP BY`, `HAVING`, `ORDER BY`, `LIMIT`, `OFFSET`
- `DISTINCT ON`, `FILTER`, `OVER`, named `WINDOW`, `NULLS FIRST/LAST`
- `LATERAL`, `VALUES` table sources, qualified `table.*`, alias column lists
- `FOR UPDATE` / lock clauses
- `UNION`, `INTERSECT`, `EXCEPT` (with `ALL`)
- Common Table Expressions (`WITH [RECURSIVE] ...`)
- Subqueries (scalar, `IN`, `EXISTS`, `FROM`)
- `INSERT INTO ... VALUES`, `INSERT INTO ... SELECT`
- `INSERT ... SET`, `DEFAULT VALUES`, `RETURNING`
- `INSERT ... ON DUPLICATE KEY UPDATE`
- `INSERT ... ON CONFLICT ... WHERE ... DO UPDATE/NOTHING`
- `REPLACE INTO`
- `UPDATE ... SET ... WHERE ... RETURNING`
- `DELETE FROM ... WHERE ... RETURNING`, MySQL multi-table delete

### DDL
- `CREATE TABLE` (columns, constraints, options)
- `CREATE TABLE IF NOT EXISTS`
- `CREATE TABLE ... LIKE`
- `CREATE TABLE ... AS SELECT`
- `CREATE [UNIQUE] INDEX`
- `CREATE [OR REPLACE] VIEW`, materialized views
- Generated and identity columns
- `CREATE/DROP FUNCTION`, `PROCEDURE`, `TRIGGER`, `SEQUENCE` as structured object DDL with raw body preservation
- `ALTER TABLE` — ADD/DROP/MODIFY/ALTER COLUMN, ADD/DROP CONSTRAINT, DROP INDEX, RENAME
- `DROP TABLE [IF EXISTS]`
- `DROP INDEX`
- `TRUNCATE TABLE`

### Misc
- `USE database`
- `SHOW TABLES / DATABASES [LIKE ...]`
- `EXPLAIN <statement>`
- Multi-statement parsing (`;` separated)
- Lossless document parsing with comments/whitespace preserved

### Expressions
- Arithmetic: `+`, `-`, `*`, `/`, `%`
- Comparison: `=`, `!=`, `<>`, `<`, `>`, `<=`, `>=`
- Logical: `AND`, `OR`, `NOT`
- Bitwise: `&`, `|`, `^`, `~`, `<<`, `>>`
- `BETWEEN ... AND ...`
- `[NOT] IN (list | subquery)`
- `[NOT] LIKE ... [ESCAPE ...]`
- `IS [NOT] NULL`
- `EXISTS (subquery)`
- `CASE ... WHEN ... THEN ... [ELSE ...] END`
- `CAST(expr AS type)`
- Function calls: `f()`, `f(DISTINCT expr)`, `f(*)`, named args `name => expr` / `name := expr`
- Named params: `:name`, `@name`, `$N`, `?`

---

## Installation

```bash
go get github.com/oarkflow/sqlparser
```

---

## Usage

### Parse a single statement

```go
import sqlparser "github.com/oarkflow/sqlparser"

stmt, err := sqlparser.ParseStatement(`
    SELECT u.id, COUNT(o.id) AS orders
    FROM users u
    LEFT JOIN orders o ON u.id = o.user_id
    WHERE u.active = 1
    GROUP BY u.id
    HAVING COUNT(o.id) > 0
    ORDER BY orders DESC
    LIMIT 20
`)
if err != nil {
    log.Fatal(err)
}

sel := stmt.(*sqlparser.SelectStmt)
fmt.Printf("columns: %d, tables: %d\n", len(sel.Columns), len(sel.From))
```

### Parse multiple statements

```go
stmts, err := sqlparser.ParseStatements(sql)
for _, s := range stmts {
    switch s := s.(type) {
    case *sqlparser.SelectStmt:   // ...
    case *sqlparser.InsertStmt:   // ...
    case *sqlparser.CreateTableStmt: // ...
    }
}
```

### Parse losslessly

```go
doc, err := sqlparser.ParseDocumentString("/* keep */ SELECT  1;\n")
if err != nil {
    log.Fatal(err)
}
fmt.Println(doc.SQL())              // exact original SQL
fmt.Println(doc.Statements[0].SQL()) // exact original statement slice
```

### Parse untrusted SQL with guardrails

```go
stmts, err := sqlparser.ParseStatementsWithOptions(sql, sqlparser.ParseOptions{
    MaxBytes:       1 << 20,
    MaxTokens:      100_000,
    MaxDepth:       256,
    MaxStatements:  1_000,
    AllowGenericDDL: false,
})
```

### Reuse a parser (best performance)

```go
p := sqlparser.New(nil)
for _, src := range queries {
    p.Reset([]byte(src))
    stmt, err := p.Next()
    // ...
}
```

### Tokenize only (fastest path)

```go
buf := make([]sqlparser.Token, 0, 128) // reuse across calls
tokens := sqlparser.Tokenize([]byte(sql), buf)
for _, tok := range tokens {
    fmt.Printf("%s %q\n", tok.Type, tok.Raw)
}
```

### Convert SQL to a target dialect

```go
converted, err := sqlparser.ConvertDialect(`
    INSERT INTO users (id, name) VALUES (1, IFNULL(:name, 'x'))
    ON DUPLICATE KEY UPDATE name = VALUES(name)
`, sqlparser.DialectPostgres)
if err != nil {
    log.Fatal(err)
}
fmt.Println(converted)
```

Set `Strict:true` when fallback DDL or unsupported conversion paths should return an error instead of best-effort SQL.

### Analyze SQL validity and optimization hints

```go
report := sqlparser.AnalyzeSQL(`
    UPDATE users SET active = 1;
`)
fmt.Println(report.Valid)           // true
fmt.Println(report.StatementCount)  // 1
for _, f := range report.Findings {
    fmt.Printf("[%s] %s: %s\n", f.Severity, f.Code, f.Message)
}
```

---

## Architecture

```
sqlparser/
├── sqlparser.go          # Public API + re-exports
├── lexer/
│   ├── token.go          # TokenType constants (uint8) + names
│   ├── keywords.go       # Two-level [len][firstChar] keyword lookup (O(1), 0 allocs)
│   ├── lexer.go          # State-machine lexer, charClass[256] dispatch table
│   ├── lexer_test.go     # Comprehensive lexer unit tests
│   └── fuzz_test.go      # Fuzz testing for crash safety
├── ast/
│   └── ast.go            # All AST node types (value-type heavy, cache-friendly)
└── parser/
    ├── arena.go          # Monotonic bump allocator (8 KiB initial slabs)
    ├── parser.go         # Recursive descent + Pratt expression parser
    ├── parser_test.go    # Comprehensive parser tests + benchmarks
    └── fuzz_test.go      # Fuzz testing for crash safety
```

### Keyword Lookup

Keywords use a compact hash-bucket lookup with no heap allocation on the lexer hot path.

### Expression Parser (Pratt)

Expressions use a top-down operator precedence (Pratt) parser. Each call to `parseExpr(minPrec)` loops over infix/postfix operators as long as their precedence exceeds `minPrec`. This avoids the deep mutual recursion of classic grammar-based parsers and makes operator extension trivial.

### Arena Allocator

The `arena` type maintains a linked list of byte slabs. Allocation is a single pointer bump. All AST nodes returned by a `Parser` are backed by the arena; calling `p.Reset(src)` recycles the memory without triggering GC. The default slab is 8 KiB, growing by 2x on overflow.

### Line/Column Tracking

Line and column numbers are **not tracked** during lexing or parsing. This eliminates per-byte overhead on the hot path. When an error occurs, `lexer.ComputeLineCol(src, pos)` scans the source up to the error position to compute accurate line/col. Since errors are rare, this is a significant net performance win.

---

## Running Tests & Benchmarks

```bash
# Unit tests
go test ./...

# Benchmarks
go test -bench=. -benchmem -benchtime=5s ./parser/

# Fuzz testing (lexer — run for crash safety)
go test -fuzz=FuzzLexer -fuzztime=30s ./lexer/

# Fuzz testing (parser — run for crash safety)
go test -fuzz=FuzzParser -fuzztime=30s ./parser/

# CPU profile
go test -bench=BenchmarkParseSelect -cpuprofile cpu.pprof ./parser/
go tool pprof cpu.pprof

# Run all SQL examples (parse + dialect conversion)
go run ./examples
```

The examples runner also performs SQL analysis and prints:
- Finding severity + code
- Human-friendly problem statement
- Concrete recommendation to fix/optimize

---

## License

MIT
