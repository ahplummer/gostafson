package projects

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

// Embedder turns text into vectors for meaning search. query is true for a
// search query, false for a passage being indexed (some models, such as
// nomic-embed-text, want them marked differently).
type Embedder interface {
	Embed(ctx context.Context, texts []string, query bool) ([][]float32, error)
}

// Index is a project's search index: its sources' page text, the passages
// (chunks) with their embeddings, and a full-text index over the passages.
// It lives in the project directory and is rebuilt from sources/ on Sync.
type Index struct {
	db       *sql.DB
	sources  string
	extract  *Extractor
	embedder Embedder

	mu      sync.Mutex
	vectors map[int64][]float32 // chunk id -> embedding, loaded on first search
}

const schema = `
CREATE TABLE IF NOT EXISTS sources (
	id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, sha256 TEXT NOT NULL,
	size INTEGER NOT NULL, mtime INTEGER NOT NULL, kind TEXT NOT NULL,
	pages INTEGER NOT NULL DEFAULT 0, chunks INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '', indexed_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS pages (
	source_id INTEGER NOT NULL, page INTEGER NOT NULL, text TEXT NOT NULL,
	PRIMARY KEY (source_id, page));
CREATE TABLE IF NOT EXISTS chunks (
	id INTEGER PRIMARY KEY, source_id INTEGER NOT NULL, page INTEGER NOT NULL,
	text TEXT NOT NULL, embedding BLOB);
CREATE INDEX IF NOT EXISTS chunks_source ON chunks(source_id);
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(text, chunk_id UNINDEXED, tokenize='porter unicode61');
`

// OpenIndex opens (creating) the index at path for the sources directory.
func OpenIndex(path, sources string, x *Extractor, e Embedder) (*Index, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("index %s: %w", path, err)
	}
	os.Chmod(path, 0o600)
	return &Index{db: db, sources: sources, extract: x, embedder: e}, nil
}

// Close closes the index.
func (ix *Index) Close() error { return ix.db.Close() }

// Source is one indexed source.
type Source struct {
	Name   string
	Kind   string
	Pages  int
	Chunks int
	Error  string // why it couldn't be read, if it couldn't
}

// Sources lists the indexed sources by name.
func (ix *Index) Sources(ctx context.Context) ([]Source, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT name, kind, pages, chunks, error FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Source
	for rows.Next() {
		var s Source
		if err := rows.Scan(&s.Name, &s.Kind, &s.Pages, &s.Chunks, &s.Error); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SyncReport says what Sync did.
type SyncReport struct {
	Added, Updated, Removed []string
	Failed                  map[string]string
}

// Changed reports whether Sync changed anything.
func (r SyncReport) Changed() bool {
	return len(r.Added)+len(r.Updated)+len(r.Removed)+len(r.Failed) > 0
}

// Sync brings the index up to date with sources/: new and changed files
// (by size and modification time, then SHA-256) are extracted, chunked and
// embedded; files that are gone are dropped. progress, if set, is called
// before each file is indexed.
func (ix *Index) Sync(ctx context.Context, progress func(name string, n, total int)) (SyncReport, error) {
	rep := SyncReport{Failed: map[string]string{}}
	type file struct {
		name  string
		size  int64
		mtime int64
	}
	var files []file
	err := filepath.WalkDir(ix.sources, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".") || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(ix.sources, p)
		files = append(files, file{rel, info.Size(), info.ModTime().UnixNano()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return rep, err
	}

	known := map[string]struct {
		sha         string
		size, mtime int64
	}{}
	rows, err := ix.db.QueryContext(ctx, `SELECT name, sha256, size, mtime FROM sources`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var n, sha string
		var size, mtime int64
		rows.Scan(&n, &sha, &size, &mtime)
		known[n] = struct {
			sha         string
			size, mtime int64
		}{sha, size, mtime}
	}
	rows.Close()

	var todo []file
	present := map[string]bool{}
	for _, f := range files {
		present[f.name] = true
		if k, ok := known[f.name]; !ok || k.size != f.size || k.mtime != f.mtime {
			todo = append(todo, f)
		}
	}
	for n := range known {
		if !present[n] {
			if err := ix.drop(ctx, n); err != nil {
				return rep, err
			}
			rep.Removed = append(rep.Removed, n)
		}
	}
	for i, f := range todo {
		if progress != nil {
			progress(f.name, i+1, len(todo))
		}
		path := filepath.Join(ix.sources, f.name)
		sha, err := fileSHA(path)
		if err != nil {
			rep.Failed[f.name] = err.Error()
			continue
		}
		if k, ok := known[f.name]; ok && k.sha == sha { // touched, not changed
			ix.db.ExecContext(ctx, `UPDATE sources SET size = ?, mtime = ? WHERE name = ?`, f.size, f.mtime, f.name)
			continue
		}
		if err := ix.add(ctx, f.name, path, sha, f.size, f.mtime); err != nil {
			rep.Failed[f.name] = err.Error()
			continue
		}
		if _, ok := known[f.name]; ok {
			rep.Updated = append(rep.Updated, f.name)
		} else {
			rep.Added = append(rep.Added, f.name)
		}
	}
	if rep.Changed() {
		ix.mu.Lock()
		ix.vectors = nil
		ix.mu.Unlock()
	}
	return rep, nil
}

func fileSHA(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (ix *Index) drop(ctx context.Context, name string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM sources WHERE name = ?`, name).Scan(&id); err != nil {
		return nil
	}
	for _, q := range []string{
		`DELETE FROM chunks_fts WHERE chunk_id IN (SELECT id FROM chunks WHERE source_id = ?)`,
		`DELETE FROM chunks WHERE source_id = ?`,
		`DELETE FROM pages WHERE source_id = ?`,
		`DELETE FROM sources WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// add (re)indexes one source. A source that can't be read is still recorded,
// with its error, so it isn't retried until it changes.
func (ix *Index) add(ctx context.Context, name, path, sha string, size, mtime int64) error {
	pages, xerr := ix.extract.Extract(ctx, path)
	chunks := Chunks(pages)
	var vecs [][]float32
	if ix.embedder != nil && len(chunks) > 0 {
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = name + "\n" + c.Text // the file name often says what the passage is about
		}
		for i := 0; i < len(texts); i += 32 {
			v, err := ix.embedder.Embed(ctx, texts[i:min(i+32, len(texts))], false)
			if err != nil {
				vecs = nil // keyword search still works; meaning search skips this source
				break
			}
			vecs = append(vecs, v...)
		}
	}
	if err := ix.drop(ctx, name); err != nil {
		return err
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	errText := ""
	if xerr != nil {
		errText = xerr.Error()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO sources (name, sha256, size, mtime, kind, pages, chunks, error, indexed_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		name, sha, size, mtime, Kind(name), len(pages), len(chunks), errText, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	for _, p := range pages {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO pages (source_id, page, text) VALUES (?, ?, ?)`, id, p.Number, p.Text); err != nil {
			return err
		}
	}
	for i, c := range chunks {
		var blob []byte
		if i < len(vecs) {
			blob = encodeVector(vecs[i])
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO chunks (source_id, page, text, embedding) VALUES (?, ?, ?, ?)`, id, c.Page, c.Text, blob)
		if err != nil {
			return err
		}
		cid, _ := r.LastInsertId()
		if _, err := tx.ExecContext(ctx, `INSERT INTO chunks_fts (text, chunk_id) VALUES (?, ?)`, name+"\n"+c.Text, cid); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return xerr
}

func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

func decodeVector(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// Hit is one search result.
type Hit struct {
	Source string
	Page   int
	Text   string
}

// Search finds the k passages that best match query, merging keyword
// (BM25) and meaning (embedding) rankings by reciprocal rank fusion. Without
// an embedder, or if embedding the query fails, it is keyword search only.
func (ix *Index) Search(ctx context.Context, query string, k int) ([]Hit, error) {
	const depth = 30
	scores := map[int64]float64{}
	add := func(ids []int64) {
		for rank, id := range ids {
			scores[id] += 1 / float64(60+rank)
		}
	}
	kw, err := ix.keyword(ctx, query, depth)
	if err != nil {
		return nil, err
	}
	add(kw)
	if ix.embedder != nil {
		if v, err := ix.embedder.Embed(ctx, []string{query}, true); err == nil && len(v) == 1 {
			add(ix.nearest(ctx, v[0], depth))
		}
	}
	ids := make([]int64, 0, len(scores))
	for id := range scores {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if scores[ids[i]] != scores[ids[j]] {
			return scores[ids[i]] > scores[ids[j]]
		}
		return ids[i] < ids[j]
	})
	var hits []Hit
	for _, id := range ids[:min(k, len(ids))] {
		var h Hit
		err := ix.db.QueryRowContext(ctx, `SELECT s.name, c.page, c.text FROM chunks c JOIN sources s ON s.id = c.source_id WHERE c.id = ?`, id).
			Scan(&h.Source, &h.Page, &h.Text)
		if err == nil {
			hits = append(hits, h)
		}
	}
	return hits, nil
}

// keyword returns chunk ids matching any of the query's words, best first.
func (ix *Index) keyword(ctx context.Context, query string, n int) ([]int64, error) {
	var terms []string
	for _, w := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) >= 2 && !stopWords[w] && !slices.Contains(terms, `"`+w+`"`) {
			terms = append(terms, `"`+w+`"`) // quoted: FTS5 syntax in a query is just text
		}
	}
	if len(terms) == 0 {
		return nil, nil
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT chunk_id FROM chunks_fts WHERE chunks_fts MATCH ? ORDER BY bm25(chunks_fts) LIMIT ?`,
		strings.Join(terms, " OR "), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

var stopWords = map[string]bool{
	"the": true, "and": true, "or": true, "of": true, "to": true, "in": true, "is": true, "it": true,
	"what": true, "how": true, "do": true, "does": true, "can": true, "my": true, "for": true, "on": true,
	"this": true, "that": true, "with": true, "should": true, "use": true, "a": true, "an": true, "be": true,
	"are": true, "if": true, "me": true, "you": true, "kind": true, "which": true, "into": true, "mean": true,
}

// nearest returns the n chunk ids whose embeddings are closest to v
// (cosine). Vectors are loaded once and kept until the next Sync changes
// something: a few thousand passages fit easily in memory.
func (ix *Index) nearest(ctx context.Context, v []float32, n int) []int64 {
	ix.mu.Lock()
	if ix.vectors == nil {
		ix.vectors = map[int64][]float32{}
		rows, err := ix.db.QueryContext(ctx, `SELECT id, embedding FROM chunks WHERE embedding IS NOT NULL`)
		if err == nil {
			for rows.Next() {
				var id int64
				var b []byte
				rows.Scan(&id, &b)
				ix.vectors[id] = normalize(decodeVector(b))
			}
			rows.Close()
		}
	}
	vecs := ix.vectors
	ix.mu.Unlock()
	q := normalize(v)
	type scored struct {
		id int64
		s  float32
	}
	var all []scored
	for id, e := range vecs {
		if len(e) != len(q) {
			continue
		}
		var dot float32
		for i := range q {
			dot += q[i] * e[i]
		}
		all = append(all, scored{id, dot})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].s > all[j].s })
	ids := make([]int64, 0, n)
	for _, s := range all[:min(n, len(all))] {
		ids = append(ids, s.id)
	}
	return ids
}

func normalize(v []float32) []float32 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	if sum == 0 {
		return v
	}
	inv := float32(1 / math.Sqrt(sum))
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = f * inv
	}
	return out
}

// ReadPage returns a page's text (page 0 for sources without pages).
func (ix *Index) ReadPage(ctx context.Context, source string, page int) (string, int, error) {
	var text string
	var pages int
	err := ix.db.QueryRowContext(ctx, `SELECT p.text, s.pages FROM pages p JOIN sources s ON s.id = p.source_id WHERE s.name = ? AND p.page = ?`,
		source, page).Scan(&text, &pages)
	if err == sql.ErrNoRows {
		err = ix.db.QueryRowContext(ctx, `SELECT pages FROM sources WHERE name = ?`, source).Scan(&pages)
		if err == sql.ErrNoRows {
			return "", 0, fmt.Errorf("no source named %q", source)
		}
		return "", pages, fmt.Errorf("%s has no page %d (it has %d)", source, page, pages)
	}
	return text, pages, err
}
