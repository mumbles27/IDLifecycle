// Package connstr holds what the store and the SQL sources share about connection strings:
// scrubbing passwords out of error messages, the IBM Db2 CLI keywords both append to ODBC
// connection strings, and a bounded connect for the ODBC driver (which ignores contexts while
// connecting).
package connstr

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ---- scrubbing -------------------------------------------------------------

// Scrub removes the DSN and any password inside it from an error message, in case a driver
// echoes its connection string. The original error chain is dropped on purpose (only when
// something was removed).
func Scrub(err error, dsn string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	needles := []string{dsn}
	needles = append(needles, Passwords(dsn)...)
	changed := false
	for _, s := range needles {
		if len(s) >= 3 && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "***")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

// passwordKV matches password=..., pwd=... and (Db2 CLI) newpwd=... in key=value connection
// strings, case-insensitively. The value may be quoted, ODBC-braced ({...} where "}}" is an
// escaped "}" and ';' is literal), or bare: up to ';' for ODBC (spaces allowed), or up to
// whitespace for libpq-style strings - both readings are scrubbed.
var passwordKV = regexp.MustCompile(`(?i)(?:^|[;\s])(?:password|pwd|newpwd)\s*=\s*("[^"]*"|'[^']*'|\{(?:[^}]|\}\})*\}|[^;]*)`)

// Passwords extracts password candidates from URL (user:pass@), mysql (user:pass@tcp(...))
// and key=value (password=...; Pwd={...};) style connection strings, raw and percent-decoded.
func Passwords(dsn string) []string {
	var out []string
	add := func(p string) {
		if p != "" {
			out = append(out, p)
			if dec, err := url.PathUnescape(p); err == nil && dec != p {
				out = append(out, dec)
			}
		}
	}
	rest := dsn
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if at := strings.LastIndex(rest, "@"); at > 0 {
		if _, pass, ok := strings.Cut(rest[:at], ":"); ok {
			add(pass)
		}
	}
	for _, m := range passwordKV.FindAllStringSubmatch(dsn, -1) {
		v := m[1]
		switch {
		case len(v) >= 2 && v[0] == '{':
			inner := v[1 : len(v)-1]
			add(inner)
			if un := strings.ReplaceAll(inner, "}}", "}"); un != inner {
				add(un)
			}
		case len(v) >= 2 && (v[0] == '"' || v[0] == '\''):
			add(v[1 : len(v)-1])
		default:
			v = strings.TrimSpace(v)
			add(v)
			if f := strings.Fields(v); len(f) > 1 {
				add(f[0])
			}
		}
	}
	return out
}

// ---- ODBC connection strings -----------------------------------------------

// ODBCValue returns the value of key in an ODBC connection string ("k=v;k={v;with;semicolons};"),
// matching the key case-insensitively and ignoring spaces around it. A braced value is returned
// without its braces ("}}" unescaped); other values are returned exactly as written.
func ODBCValue(dsn, key string) (string, bool) {
	want := strings.ToLower(strings.TrimSpace(key))
	for i := 0; i < len(dsn); {
		end := strings.IndexByte(dsn[i:], ';')
		eq := strings.IndexByte(dsn[i:], '=')
		if eq < 0 || (end >= 0 && end < eq) { // a segment without '=': skip it
			if end < 0 {
				break
			}
			i += end + 1
			continue
		}
		k := strings.ToLower(strings.TrimSpace(dsn[i : i+eq]))
		j := i + eq + 1
		// A value starting with '{' (after spaces) runs to the matching '}'.
		vs := j
		for vs < len(dsn) && dsn[vs] == ' ' {
			vs++
		}
		var v string
		if vs < len(dsn) && dsn[vs] == '{' {
			p := vs + 1
			var b strings.Builder
			for p < len(dsn) {
				if dsn[p] == '}' {
					if p+1 < len(dsn) && dsn[p+1] == '}' {
						b.WriteByte('}')
						p += 2
						continue
					}
					break
				}
				b.WriteByte(dsn[p])
				p++
			}
			v = b.String()
			j = p + 1
			if e := strings.IndexByte(dsn[min(j, len(dsn)):], ';'); e >= 0 {
				j += e
			} else {
				j = len(dsn)
			}
		} else if e := strings.IndexByte(dsn[j:], ';'); e >= 0 {
			v, j = dsn[j:j+e], j+e
		} else {
			v, j = dsn[j:], len(dsn)
		}
		if k == want {
			return v, true
		}
		i = j + 1
	}
	return "", false
}

// DB2ConnectTimeout is the ConnectTimeout (seconds) appended to Db2 connection strings that do
// not set one.
const DB2ConnectTimeout = 20

// AugmentDB2 appends the IBM Db2 CLI/ODBC keywords that github.com/alexbrainman/odbc needs,
// unless the connection string already sets them:
//   - LongDataCompat=1 reports CLOB as SQL_LONGVARCHAR; the driver package does not know
//     SQL_CLOB (-99) and would fail with "unsupported column type -99".
//   - MapCharToWChar=1 describes CHAR/VARCHAR/LONGVARCHAR as the wide (UTF-16) types, so text
//     arrives as Go strings instead of bytes in the client's ANSI code page. MapCharToWChar=0
//     is refused: non-ASCII text would be silently garbled.
//   - ConnectTimeout=<connectTimeout> (IBM CLI keyword, seconds; the default 0 waits forever)
//     makes the driver itself give up on an unreachable server.
//
// The connection string is not otherwise changed: values are never trimmed, and only trailing
// whitespace is ignored when deciding whether a ';' separator is needed.
func AugmentDB2(dsn string, connectTimeout int) (string, error) {
	if v, ok := ODBCValue(dsn, "MapCharToWChar"); ok && strings.TrimSpace(v) != "1" {
		return "", fmt.Errorf("the connection string sets MapCharToWChar=%s; remove it (MapCharToWChar=1 is required so non-ASCII text is not garbled)", strings.TrimSpace(v))
	}
	out := dsn
	if t := strings.TrimRight(dsn, " \t\r\n"); t == "" {
		out = ""
	} else if strings.HasSuffix(t, ";") {
		out = t
	} else {
		out += ";"
	}
	for _, kv := range [][2]string{{"LongDataCompat", "1"}, {"MapCharToWChar", "1"}, {"ConnectTimeout", strconv.Itoa(connectTimeout)}} {
		if _, ok := ODBCValue(dsn, kv[0]); !ok {
			out += kv[0] + "=" + kv[1] + ";"
		}
	}
	return out, nil
}
