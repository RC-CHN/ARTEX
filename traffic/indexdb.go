package traffic

import (
	"database/sql"
	"strconv"
	"strings"
)

// indexDB keeps the shared query code in one place. SQLite remains a reference
// backend for old-file migrations and benchmarks; production uses PostgreSQL.
type indexDB struct {
	*sql.DB
	postgres bool
}

func bindIndexSQL(query string, postgres bool) string {
	if !postgres {
		return query
	}
	// All statements here are application-owned SQL, never user input. Respect
	// quoted literals while rebinding placeholders (including ESCAPE '\').
	var out strings.Builder
	quoted := false
	parameter := 0
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			if quoted && i+1 < len(query) && query[i+1] == '\'' {
				out.WriteString("''")
				i++
				continue
			}
			quoted = !quoted
		}
		if ch == '?' && !quoted {
			parameter++
			out.WriteByte('$')
			out.WriteString(strconv.Itoa(parameter))
		} else if !quoted && strings.HasPrefix(query[i:], " LIKE ") {
			// SQLite LIKE is case-insensitive for ASCII. Retain that behavior.
			out.WriteString(" ILIKE ")
			i += len(" LIKE ") - 1
		} else {
			out.WriteByte(ch)
		}
	}
	return out.String()
}

func (d *indexDB) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(bindIndexSQL(query, d.postgres), args...)
}

func (d *indexDB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.DB.Query(bindIndexSQL(query, d.postgres), args...)
}

func (d *indexDB) QueryRow(query string, args ...any) *sql.Row {
	return d.DB.QueryRow(bindIndexSQL(query, d.postgres), args...)
}

func (d *indexDB) Begin() (*indexTx, error) {
	tx, err := d.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &indexTx{Tx: tx, postgres: d.postgres}, nil
}

type indexTx struct {
	*sql.Tx
	postgres bool
}

func (tx *indexTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(bindIndexSQL(query, tx.postgres), args...)
}

func (tx *indexTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRow(bindIndexSQL(query, tx.postgres), args...)
}

func literalLike(term string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(term) + "%"
}

// PostgreSQL text cannot hold NUL or invalid UTF-8. Only the derived search
// document is normalized; original headers and bodies are stored as bytea.
func searchText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}

func (t *Traffic) headerValue(header string) any {
	if t.db.postgres {
		return []byte(header)
	}
	return header
}
