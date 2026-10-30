// Package backup writes and reads IDLifecycle backups (.idlbak): the application database (every table except
// sessions), config.json and the master key, encrypted with a passphrase.
//
// File layout:
//
//	IDLBAK1\n
//	<header JSON>\n        not secret: format, time, app version, driver, schema version, row counts, KDF parameters
//	<chunk>...             AES-256-GCM, key = scrypt(passphrase, salt); each chunk is a 4-byte big-endian length and
//	                       its ciphertext; the nonce is a random prefix + chunk counter + a last-chunk flag and the
//	                       header's SHA-256 is the additional data, so a reordered, truncated, extended or re-headed
//	                       file does not decrypt.
//
// The decrypted stream is a tar archive: manifest.json, config.json, master.key, tables/<table>.jsonl (one JSON object
// per row) in store.BackupTables order. Tables are exported row by row, so the database's size, not memory, limits
// a backup. Because the content is plain rows, a backup can be restored into any supported database driver.
package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"

	"idlifecycle/internal/store"
)

const (
	magic         = "IDLBAK1"
	formatVersion = 1
	chunkSize     = 64 << 10
	// MinPassphrase is the shortest accepted backup passphrase.
	MinPassphrase = 12
	maxHeader     = 64 << 10
	maxMember     = 16 << 20 // manifest/config/key members are small; tables stream
)

// Extension is the file name extension of a backup.
const Extension = ".idlbak"

// ErrPassphrase is returned when the passphrase is wrong (or the file was changed).
var ErrPassphrase = errors.New("the backup cannot be decrypted: wrong passphrase, or the file was changed or damaged")

// KDF holds the scrypt parameters and salt.
type KDF struct {
	Name string `json:"name"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
	Salt []byte `json:"salt"`
}

// Header is the readable first part of a backup file.
type Header struct {
	Format      int            `json:"format"`
	Created     time.Time      `json:"created"`
	AppVersion  string         `json:"app_version"`
	Driver      string         `json:"driver"`
	Schema      int            `json:"schema"`
	Tables      map[string]int `json:"tables"`
	Note        string         `json:"note,omitempty"`
	KDF         KDF            `json:"kdf"`
	NoncePrefix []byte         `json:"nonce_prefix"`
}

// Manifest is the encrypted description of the content (authoritative row counts).
type Manifest struct {
	Created    time.Time      `json:"created"`
	AppVersion string         `json:"app_version"`
	Driver     string         `json:"driver"`
	Schema     int            `json:"schema"`
	Tables     map[string]int `json:"tables"`
	HasConfig  bool           `json:"has_config"`
}

// Options for Create.
type Options struct {
	Passphrase string
	AppVersion string
	ConfigJSON []byte // config.json as it is on disk (may be empty)
	Key        []byte // the 32-byte master key
	Note       string
	Now        func() time.Time
	// scrypt cost (tests lower it); 0 = default 1<<15.
	ScryptN int
}

// Create writes a backup of st to w and returns its header (with the real row counts).
func Create(ctx context.Context, w io.Writer, st *store.Store, o Options) (Header, error) {
	if len([]rune(o.Passphrase)) < MinPassphrase {
		return Header{}, fmt.Errorf("the backup passphrase must be at least %d characters", MinPassphrase)
	}
	if len(o.Key) != 32 {
		return Header{}, errors.New("backup: the master key must be 32 bytes")
	}
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	counts, err := st.DataRows(ctx)
	if err != nil {
		return Header{}, err
	}
	n := o.ScryptN
	if n == 0 {
		n = 1 << 15
	}
	h := Header{
		Format: formatVersion, Created: now().UTC(), AppVersion: o.AppVersion, Driver: st.Driver(),
		Schema: store.SchemaVersion(), Tables: counts, Note: o.Note,
		KDF: KDF{Name: "scrypt", N: n, R: 8, P: 1, Salt: random(16)}, NoncePrefix: random(7),
	}
	headerJSON, err := json.Marshal(h)
	if err != nil {
		return Header{}, err
	}
	if _, err := io.WriteString(w, magic+"\n"); err != nil {
		return Header{}, err
	}
	if _, err := w.Write(append(headerJSON, '\n')); err != nil {
		return Header{}, err
	}

	aead, err := deriveAEAD(o.Passphrase, h.KDF)
	if err != nil {
		return Header{}, err
	}
	sw := &sealWriter{w: w, aead: aead, prefix: h.NoncePrefix, ad: hashOf(headerJSON)}
	tw := tar.NewWriter(sw)

	// The manifest (with the row counts actually written) comes last.
	man := Manifest{Created: h.Created, AppVersion: h.AppVersion, Driver: h.Driver, Schema: h.Schema, Tables: map[string]int{}, HasConfig: len(o.ConfigJSON) > 0}
	if err := addBytes(tw, "config.json", o.ConfigJSON, h.Created); err != nil {
		return Header{}, err
	}
	if err := addBytes(tw, "master.key", o.Key, h.Created); err != nil {
		return Header{}, err
	}
	for _, table := range store.BackupTables {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		// tar needs a member's size before its data, so rows are collected in memory and written as numbered parts
		// of about 8 MB each (tables/<table>.0000.jsonl, .0001, ...); a big table never has to fit in memory.
		part := 0
		rowsInPart := 0
		flush := func() error {
			if rowsInPart == 0 && part > 0 {
				return nil
			}
			name := fmt.Sprintf("tables/%s.%04d.jsonl", table, part)
			if err := addBytes(tw, name, buf.Bytes(), h.Created); err != nil {
				return err
			}
			buf.Reset()
			part++
			rowsInPart = 0
			return nil
		}
		cnt, err := st.ExportTable(ctx, table, func(row map[string]any) error {
			if err := enc.Encode(row); err != nil {
				return err
			}
			rowsInPart++
			if buf.Len() >= 8<<20 {
				return flush()
			}
			return nil
		})
		if err != nil {
			return Header{}, fmt.Errorf("export %s: %w", table, err)
		}
		if err := flush(); err != nil {
			return Header{}, err
		}
		man.Tables[table] = cnt
		h.Tables[table] = cnt
	}
	manJSON, _ := json.Marshal(man)
	if err := addBytes(tw, "manifest.json", manJSON, h.Created); err != nil {
		return Header{}, err
	}
	if err := tw.Close(); err != nil {
		return Header{}, err
	}
	if err := sw.Close(); err != nil {
		return Header{}, err
	}
	return h, nil
}

// Contents is what Restore read back besides the tables.
type Contents struct {
	Header     Header
	Manifest   Manifest
	ConfigJSON []byte
	Key        []byte
	Rows       map[string]int
}

// ReadHeader reads only the readable header (no passphrase needed).
func ReadHeader(r io.Reader) (Header, error) {
	h, _, _, err := readHeader(bufio.NewReader(r))
	return h, err
}

func readHeader(br *bufio.Reader) (Header, []byte, *bufio.Reader, error) {
	line, err := br.ReadString('\n')
	if err != nil || strings.TrimRight(line, "\r\n") != magic {
		return Header{}, nil, nil, errors.New("not an IDLifecycle backup file")
	}
	raw, err := readLine(br, maxHeader)
	if err != nil {
		return Header{}, nil, nil, fmt.Errorf("backup header: %w", err)
	}
	var h Header
	if err := json.Unmarshal(raw, &h); err != nil {
		return Header{}, nil, nil, fmt.Errorf("backup header: %w", err)
	}
	if h.Format != formatVersion {
		return Header{}, nil, nil, fmt.Errorf("backup format %d is not supported by this version (it reads format %d)", h.Format, formatVersion)
	}
	if h.KDF.Name != "scrypt" || h.KDF.N < 1<<10 || h.KDF.N > 1<<20 || h.KDF.R != 8 || h.KDF.P != 1 || len(h.KDF.Salt) < 16 || len(h.NoncePrefix) != 7 {
		return Header{}, nil, nil, errors.New("backup header: unsupported key derivation parameters")
	}
	return h, raw, br, nil
}

func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > max {
			return nil, errors.New("line too long")
		}
		if !isPrefix {
			return out, nil
		}
	}
}

// Restore decrypts r and loads its tables into st (replacing their contents, in one transaction). It refuses a
// backup made by a newer schema than this build knows. The config and key are returned for the caller to write.
func Restore(ctx context.Context, r io.Reader, passphrase string, st *store.Store) (Contents, error) {
	return restore(ctx, r, passphrase, st, nil)
}

func restore(ctx context.Context, r io.Reader, passphrase string, st *store.Store, checkKey func([]byte) error) (Contents, error) {
	h, raw, br, err := readHeader(bufio.NewReader(r))
	if err != nil {
		return Contents{}, err
	}
	if h.Schema > store.SchemaVersion() {
		return Contents{}, fmt.Errorf("the backup was made by a newer version (database schema %d, this version knows %d): install that version or newer first", h.Schema, store.SchemaVersion())
	}
	aead, err := deriveAEAD(passphrase, h.KDF)
	if err != nil {
		return Contents{}, err
	}
	or := &openReader{r: br, aead: aead, prefix: h.NoncePrefix, ad: hashOf(raw)}
	tr := tar.NewReader(or)
	c := Contents{Header: h}

	next := func() (*tar.Header, error) {
		th, err := tr.Next()
		if err != nil {
			if errors.Is(err, ErrPassphrase) {
				return nil, err
			}
			return nil, fmt.Errorf("backup content: %w", err)
		}
		return th, nil
	}
	readSmall := func(th *tar.Header) ([]byte, error) {
		if th.Size > maxMember {
			return nil, fmt.Errorf("backup member %s is too large", th.Name)
		}
		return io.ReadAll(tr)
	}

	th, err := next()
	if err != nil {
		return Contents{}, err
	}
	if th.Name != "config.json" {
		return Contents{}, fmt.Errorf("backup content: expected config.json, found %s", th.Name)
	}
	if c.ConfigJSON, err = readSmall(th); err != nil {
		return Contents{}, err
	}
	if th, err = next(); err != nil {
		return Contents{}, err
	}
	if th.Name != "master.key" {
		return Contents{}, fmt.Errorf("backup content: expected master.key, found %s", th.Name)
	}
	if c.Key, err = readSmall(th); err != nil {
		return Contents{}, err
	}
	if len(c.Key) != 32 {
		return Contents{}, errors.New("backup content: the master key is not 32 bytes")
	}
	if checkKey != nil {
		if err := checkKey(c.Key); err != nil {
			return Contents{}, err
		}
	}

	// Tables: parts of each table in BackupTables order. The tar reader is positioned on the next member.
	var pending *tar.Header
	pendingErr := error(nil)
	advance := func() {
		pending, pendingErr = next()
	}
	advance()
	seen := map[string]int{}
	lastTable := store.BackupTables[len(store.BackupTables)-1]
	// finish runs inside the import transaction, after the last table: the manifest, the row counts, the end of the
	// archive and the final chunk must all check out, or the transaction is rolled back and nothing changes.
	finish := func() error {
		if pending == nil || pending.Name != "manifest.json" {
			return errors.New("backup content: manifest.json is missing (incomplete file)")
		}
		mb, err := readSmall(pending)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(mb, &c.Manifest); err != nil {
			return fmt.Errorf("backup manifest: %w", err)
		}
		for _, table := range store.BackupTables {
			if seen[table] != c.Manifest.Tables[table] {
				return fmt.Errorf("backup %s: %d rows read, the manifest says %d", table, seen[table], c.Manifest.Tables[table])
			}
		}
		if _, err := tr.Next(); !errors.Is(err, io.EOF) {
			if err == nil {
				return errors.New("backup content: unexpected data after the manifest")
			}
			return fmt.Errorf("backup content: %w", err)
		}
		if _, err := io.Copy(io.Discard, or); err != nil {
			return err
		}
		if !or.sawLast {
			return ErrPassphrase
		}
		return nil
	}
	src := func(table string, fn func(row map[string]any) error) error {
		prefix := "tables/" + table + "."
		for pendingErr == nil && pending != nil && strings.HasPrefix(pending.Name, prefix) {
			dec := json.NewDecoder(tr)
			dec.UseNumber()
			for {
				var row map[string]any
				if err := dec.Decode(&row); err != nil {
					if errors.Is(err, io.EOF) {
						break
					}
					if errors.Is(err, ErrPassphrase) {
						return err
					}
					return fmt.Errorf("backup %s: %w", pending.Name, err)
				}
				seen[table]++
				if err := fn(row); err != nil {
					return err
				}
			}
			advance()
		}
		if pendingErr != nil {
			return pendingErr
		}
		if table == lastTable {
			return finish()
		}
		return nil
	}
	if c.Rows, err = st.ImportTables(ctx, src); err != nil {
		return Contents{}, err
	}
	return c, nil
}

func addBytes(tw *tar.Writer, name string, b []byte, mod time.Time) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(b)), ModTime: mod, Format: tar.FormatPAX}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func random(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return b
}

func hashOf(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func deriveAEAD(passphrase string, k KDF) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), k.Salt, k.N, k.R, k.P, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func nonce(prefix []byte, counter uint32, last bool) []byte {
	n := make([]byte, 12)
	copy(n, prefix)
	binary.BigEndian.PutUint32(n[7:11], counter)
	if last {
		n[11] = 1
	}
	return n
}

// sealWriter encrypts everything written to it in chunks; Close writes the final (flagged) chunk.
type sealWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	prefix  []byte
	ad      []byte
	buf     []byte
	counter uint32
	closed  bool
}

func (s *sealWriter) Write(p []byte) (int, error) {
	n := len(p)
	s.buf = append(s.buf, p...)
	for len(s.buf) > chunkSize { // keep at least one byte back so the last chunk is never empty unless all is
		if err := s.emit(s.buf[:chunkSize], false); err != nil {
			return 0, err
		}
		s.buf = append(s.buf[:0], s.buf[chunkSize:]...)
	}
	return n, nil
}

func (s *sealWriter) emit(plain []byte, last bool) error {
	if s.counter == ^uint32(0) {
		return errors.New("backup too large")
	}
	ct := s.aead.Seal(nil, nonce(s.prefix, s.counter, last), plain, s.ad)
	s.counter++
	if len(ct) > chunkSize+s.aead.Overhead() {
		return errors.New("backup chunk too large") // cannot happen: chunks are at most chunkSize
	}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct))) // #nosec G115 -- bounded by chunkSize + overhead just above
	if _, err := s.w.Write(l[:]); err != nil {
		return err
	}
	_, err := s.w.Write(ct)
	return err
}

func (s *sealWriter) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.emit(s.buf, true)
}

// openReader decrypts the chunk stream; any tampering or a wrong passphrase gives ErrPassphrase.
type openReader struct {
	r       io.Reader
	aead    cipher.AEAD
	prefix  []byte
	ad      []byte
	plain   []byte
	counter uint32
	sawLast bool
}

func (o *openReader) Read(p []byte) (int, error) {
	for len(o.plain) == 0 {
		if o.sawLast {
			return 0, io.EOF
		}
		var l [4]byte
		if _, err := io.ReadFull(o.r, l[:]); err != nil {
			return 0, ErrPassphrase // truncated: the last chunk never came
		}
		size := int(binary.BigEndian.Uint32(l[:]))
		if size < o.aead.Overhead() || size > chunkSize+o.aead.Overhead() {
			return 0, ErrPassphrase
		}
		ct := make([]byte, size)
		if _, err := io.ReadFull(o.r, ct); err != nil {
			return 0, ErrPassphrase
		}
		pt, err := o.aead.Open(nil, nonce(o.prefix, o.counter, false), ct, o.ad)
		if err != nil {
			if pt, err = o.aead.Open(nil, nonce(o.prefix, o.counter, true), ct, o.ad); err != nil {
				return 0, ErrPassphrase
			}
			o.sawLast = true
			// Nothing may follow the last chunk.
			var one [1]byte
			if n, _ := o.r.Read(one[:]); n > 0 {
				return 0, ErrPassphrase
			}
		}
		o.counter++
		o.plain = pt
	}
	n := copy(p, o.plain)
	o.plain = o.plain[n:]
	return n, nil
}
