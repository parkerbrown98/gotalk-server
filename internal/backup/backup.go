// Package backup writes and restores instance backups: a gzip-compressed tar archive
// holding a manifest, every table as PostgreSQL COPY text, and the uploaded media files.
//
// The database is read in a single REPEATABLE READ transaction, so the dump is consistent
// while the server keeps running, and nothing beyond the gotalk binary is needed (no
// pg_dump). Media is read through the storage driver, so the same archive restores into
// local disk or any S3-compatible bucket, which also makes it the way to move between
// storage backends.
//
// Archive layout, in order:
//
//	manifest.json        format, versions, tables and their columns
//	db/<table>.copy      COPY ... TO STDOUT text output
//	db.json              row counts, checked before the restore commits
//	media/<key>          uploaded files
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/parkerbrown98/gotalk-server/internal/database"
	"github.com/parkerbrown98/gotalk-server/internal/storage"
)

// Format is the archive format version.
const Format = 1

// Manifest is the first entry of every archive.
type Manifest struct {
	Format          int         `json:"format"`
	CreatedAt       time.Time   `json:"created_at"`
	GotalkVersion   string      `json:"gotalk_version"`
	SchemaVersion   int64       `json:"schema_version"`
	PostgresVersion string      `json:"postgres_version"`
	Tables          []TableInfo `json:"tables"`
	Media           bool        `json:"media" doc:"Whether media files follow the database"`
}

// TableInfo lists a table's copied columns.
type TableInfo struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

// Counts summarizes an archive's contents.
type Counts struct {
	Rows       map[string]int64 `json:"rows"`
	MediaFiles int64            `json:"media_files"`
	MediaBytes int64            `json:"media_bytes"`
}

// Options configures Write.
type Options struct {
	// Version is the running gotalk version, recorded in the manifest.
	Version string
	// Storage and MediaPrefixes select the media to include; nil Storage skips media.
	Storage       storage.Backend
	MediaPrefixes []string
	Logger        *slog.Logger
}

const gooseTable = "goose_db_version"

// Write streams a backup of the database (and media) to w.
func Write(ctx context.Context, pool *pgxpool.Pool, w io.Writer, opts Options) (Manifest, Counts, error) {
	log := logger(opts.Logger)
	st, err := database.Status(ctx, pool)
	if err != nil {
		return Manifest{}, Counts{}, err
	}
	if st.Current != st.Latest {
		return Manifest{}, Counts{}, fmt.Errorf("the database schema (version %d) does not match this gotalk (version %d); run `gotalk migrate` or use the matching gotalk version", st.Current, st.Latest)
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	counts := Counts{Rows: map[string]int64{}}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Manifest{}, counts, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	m := Manifest{Format: Format, CreatedAt: time.Now().UTC(), GotalkVersion: opts.Version, SchemaVersion: st.Current,
		Media: opts.Storage != nil}
	if err := tx.QueryRow(ctx, "SHOW server_version").Scan(&m.PostgresVersion); err != nil {
		return m, counts, err
	}
	if m.Tables, err = listTables(ctx, tx); err != nil {
		return m, counts, err
	}
	if err := writeJSON(tw, "manifest.json", m); err != nil {
		return m, counts, err
	}
	for _, t := range m.Tables {
		n, err := dumpTable(ctx, tx, tw, t)
		if err != nil {
			return m, counts, fmt.Errorf("backing up table %s: %w", t.Name, err)
		}
		counts.Rows[t.Name] = n
	}
	if err := writeJSON(tw, "db.json", counts); err != nil {
		return m, counts, err
	}
	_ = tx.Rollback(ctx)
	log.Info("database backed up", "tables", len(m.Tables), "schema_version", m.SchemaVersion)

	if opts.Storage != nil {
		for _, prefix := range opts.MediaPrefixes {
			err := opts.Storage.List(ctx, prefix, func(obj storage.Object) error {
				if err := copyObject(ctx, opts.Storage, tw, obj); err != nil {
					return fmt.Errorf("backing up %s: %w", obj.Key, err)
				}
				counts.MediaFiles++
				counts.MediaBytes += obj.Size
				return nil
			})
			if err != nil {
				return m, counts, err
			}
		}
		log.Info("media backed up", "files", counts.MediaFiles, "bytes", counts.MediaBytes, "storage", opts.Storage.Describe())
	}
	if err := tw.Close(); err != nil {
		return m, counts, err
	}
	return m, counts, gz.Close()
}

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return l
}

func listTables(ctx context.Context, tx pgx.Tx) ([]TableInfo, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.relname,
		       array_agg(a.attname::text ORDER BY a.attnum)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND c.relname <> $1
		GROUP BY c.relname
		ORDER BY c.relname`, gooseTable)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (TableInfo, error) {
		var t TableInfo
		err := r.Scan(&t.Name, &t.Columns)
		return t, err
	})
}

func copySQL(t TableInfo, direction string) string {
	cols := make([]string, len(t.Columns))
	for i, c := range t.Columns {
		cols[i] = pgx.Identifier{c}.Sanitize()
	}
	return fmt.Sprintf("COPY %s (%s) %s", pgx.Identifier{t.Name}.Sanitize(), strings.Join(cols, ", "), direction)
}

// dumpTable copies a table into a temporary file first, since tar needs each entry's size
// up front.
func dumpTable(ctx context.Context, tx pgx.Tx, tw *tar.Writer, t TableInfo) (int64, error) {
	f, err := os.CreateTemp("", "gotalk-backup-*.copy")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	tag, err := tx.Conn().PgConn().CopyTo(ctx, f, copySQL(t, "TO STDOUT"))
	if err != nil {
		return 0, err
	}
	size, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	if err := tw.WriteHeader(&tar.Header{Name: "db/" + t.Name + ".copy", Mode: 0o600, Size: size, ModTime: time.Now()}); err != nil {
		return 0, err
	}
	if _, err := io.CopyN(tw, f, size); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func copyObject(ctx context.Context, b storage.Backend, tw *tar.Writer, obj storage.Object) error {
	rc, _, err := b.Get(ctx, obj.Key)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	mod := obj.ModTime
	if mod.IsZero() {
		mod = time.Now()
	}
	if err := tw.WriteHeader(&tar.Header{Name: "media/" + obj.Key, Mode: 0o600, Size: obj.Size, ModTime: mod}); err != nil {
		return err
	}
	n, err := io.Copy(tw, io.LimitReader(rc, obj.Size))
	if err == nil && n != obj.Size {
		err = fmt.Errorf("expected %d bytes, read %d (the file changed during the backup?)", obj.Size, n)
	}
	return err
}

func writeJSON(tw *tar.Writer, name string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err = tw.Write(data)
	return err
}

// RestoreOptions configures Restore.
type RestoreOptions struct {
	// Force replaces an existing instance's data. Without it, Restore only writes into an
	// empty database (or one that was migrated but never set up).
	Force bool
	// MediaTarget returns the storage that receives the media; it is called after the
	// database is restored, so settings saved in the restored database can apply. Nil
	// skips media.
	MediaTarget func(context.Context) (storage.Backend, error)
	Logger      *slog.Logger
}

// ErrNotEmpty is returned when the target database already holds an instance.
var ErrNotEmpty = errors.New("the target database already contains a Gotalk instance; stop the server and pass --force to replace it")

// Verify reads a whole archive and checks it is complete and consistent: the manifest,
// every table's data with the recorded row count (COPY text output has one line per
// row), valid media keys, and the gzip checksum. Restore runs it before touching the
// database, so a damaged archive never destroys existing data.
func Verify(r io.Reader) (Manifest, Counts, error) {
	counts := Counts{Rows: map[string]int64{}}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, counts, fmt.Errorf("not a gotalk backup (expected a .tar.gz archive): %w", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" {
		return Manifest{}, counts, errors.New("not a gotalk backup: manifest.json must be the first entry")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return m, counts, fmt.Errorf("reading the manifest: %w", err)
	}
	if m.Format != Format {
		return m, counts, fmt.Errorf("unsupported backup format %d (this gotalk reads format %d)", m.Format, Format)
	}
	tables := map[string]bool{}
	for _, t := range m.Tables {
		tables[t.Name] = true
	}
	var recorded *Counts
	seen := map[string]bool{}
	buf := make([]byte, 64<<10)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, counts, fmt.Errorf("the backup is damaged or truncated: %w", err)
		}
		switch {
		case recorded == nil && hdr.Name == "db.json":
			recorded = &Counts{}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(recorded); err != nil {
				return m, counts, fmt.Errorf("reading db.json: %w", err)
			}
		case recorded == nil && strings.HasPrefix(hdr.Name, "db/") && strings.HasSuffix(hdr.Name, ".copy"):
			name := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "db/"), ".copy")
			if !tables[name] || seen[name] {
				return m, counts, fmt.Errorf("unexpected table data %q", hdr.Name)
			}
			seen[name] = true
			var lines int64
			for {
				n, err := tr.Read(buf)
				for _, b := range buf[:n] {
					if b == '\n' {
						lines++
					}
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return m, counts, fmt.Errorf("the backup is damaged or truncated: %w", err)
				}
			}
			counts.Rows[name] = lines
		case recorded != nil && strings.HasPrefix(hdr.Name, "media/"):
			key := strings.TrimPrefix(hdr.Name, "media/")
			if hdr.Typeflag != tar.TypeReg || !storage.ValidKey(key) {
				return m, counts, fmt.Errorf("unexpected entry %q in the media section", hdr.Name)
			}
			n, err := io.CopyN(io.Discard, tr, hdr.Size)
			if err != nil {
				return m, counts, fmt.Errorf("the backup is damaged or truncated: %w", err)
			}
			counts.MediaFiles++
			counts.MediaBytes += n
		default:
			return m, counts, fmt.Errorf("unexpected entry %q", hdr.Name)
		}
	}
	if recorded == nil {
		return m, counts, errors.New("the backup is truncated: db.json is missing")
	}
	for _, t := range m.Tables {
		if !seen[t.Name] {
			return m, counts, fmt.Errorf("the backup is missing data for table %s", t.Name)
		}
		if counts.Rows[t.Name] != recorded.Rows[t.Name] {
			return m, counts, fmt.Errorf("table %s has %d rows but the backup recorded %d", t.Name, counts.Rows[t.Name], recorded.Rows[t.Name])
		}
	}
	return m, counts, nil
}

// Restore loads an archive into the database (and media into storage). The archive is
// verified first (see Verify), then read again to restore it. The database is restored
// in one transaction: foreign keys are dropped, every table is loaded, row counts are
// compared with the archive, and the foreign keys are re-created (which validates every
// reference) before committing. Afterwards the schema is migrated to this build's version.
func Restore(ctx context.Context, pool *pgxpool.Pool, r io.ReadSeeker, opts RestoreOptions) (Manifest, Counts, error) {
	if m, counts, err := Verify(r); err != nil {
		return m, counts, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return Manifest{}, Counts{}, err
	}
	return restore(ctx, pool, r, opts)
}

func restore(ctx context.Context, pool *pgxpool.Pool, r io.Reader, opts RestoreOptions) (Manifest, Counts, error) {
	log := logger(opts.Logger)
	counts := Counts{Rows: map[string]int64{}}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return Manifest{}, counts, fmt.Errorf("not a gotalk backup (expected a .tar.gz archive): %w", err)
	}
	tr := tar.NewReader(gz)

	hdr, err := tr.Next()
	if err != nil || hdr.Name != "manifest.json" {
		return Manifest{}, counts, errors.New("not a gotalk backup: manifest.json must be the first entry")
	}
	var m Manifest
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil {
		return m, counts, fmt.Errorf("reading the manifest: %w", err)
	}
	if m.Format != Format {
		return m, counts, fmt.Errorf("unsupported backup format %d (this gotalk reads format %d)", m.Format, Format)
	}
	latest, err := database.LatestVersion()
	if err != nil {
		return m, counts, err
	}
	if m.SchemaVersion > latest || m.SchemaVersion < 1 {
		return m, counts, fmt.Errorf("the backup has schema version %d but this gotalk knows versions up to %d; restore with gotalk %s or newer",
			m.SchemaVersion, latest, m.GotalkVersion)
	}

	if err := prepareTarget(ctx, pool, opts.Force, log); err != nil {
		return m, counts, err
	}
	if err := database.MigrateTo(ctx, pool, m.SchemaVersion, log); err != nil {
		return m, counts, err
	}

	tables := map[string]TableInfo{}
	for _, t := range m.Tables {
		tables[t.Name] = t
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return m, counts, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	fks, err := dropForeignKeys(ctx, tx)
	if err != nil {
		return m, counts, err
	}
	for _, t := range m.Tables {
		if _, err := tx.Exec(ctx, "TRUNCATE "+pgx.Identifier{t.Name}.Sanitize()); err != nil {
			return m, counts, fmt.Errorf("the backup's table %s does not exist at schema version %d: %w", t.Name, m.SchemaVersion, err)
		}
	}

	var expected *Counts
	for expected == nil {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return m, counts, errors.New("the backup is truncated: db.json is missing")
		}
		if err != nil {
			return m, counts, fmt.Errorf("reading the backup: %w", err)
		}
		switch {
		case hdr.Name == "db.json":
			expected = &Counts{}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(expected); err != nil {
				return m, counts, fmt.Errorf("reading db.json: %w", err)
			}
		case strings.HasPrefix(hdr.Name, "db/") && strings.HasSuffix(hdr.Name, ".copy"):
			name := strings.TrimSuffix(strings.TrimPrefix(hdr.Name, "db/"), ".copy")
			t, ok := tables[name]
			if !ok {
				return m, counts, fmt.Errorf("the backup contains data for unknown table %q", name)
			}
			tag, err := tx.Conn().PgConn().CopyFrom(ctx, tr, copySQL(t, "FROM STDIN"))
			if err != nil {
				return m, counts, fmt.Errorf("restoring table %s: %w", name, err)
			}
			counts.Rows[name] = tag.RowsAffected()
		default:
			return m, counts, fmt.Errorf("unexpected entry %q in the database section", hdr.Name)
		}
	}
	for _, t := range m.Tables {
		if counts.Rows[t.Name] != expected.Rows[t.Name] {
			return m, counts, fmt.Errorf("table %s: restored %d rows but the backup recorded %d", t.Name, counts.Rows[t.Name], expected.Rows[t.Name])
		}
	}
	for _, fk := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
			pgx.Identifier{fk.table}.Sanitize(), pgx.Identifier{fk.name}.Sanitize(), fk.def)); err != nil {
			return m, counts, fmt.Errorf("re-creating foreign key %s on %s: %w", fk.name, fk.table, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return m, counts, err
	}
	log.Info("database restored", "tables", len(m.Tables), "schema_version", m.SchemaVersion)
	if err := database.Migrate(ctx, pool, log); err != nil {
		return m, counts, err
	}

	var target storage.Backend
	if opts.MediaTarget != nil && m.Media {
		if target, err = opts.MediaTarget(ctx); err != nil {
			return m, counts, fmt.Errorf("the database was restored, but media could not be: %w", err)
		}
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, counts, fmt.Errorf("reading media from the backup: %w", err)
		}
		key, ok := strings.CutPrefix(hdr.Name, "media/")
		if !ok || hdr.Typeflag != tar.TypeReg || !storage.ValidKey(key) {
			return m, counts, fmt.Errorf("unexpected entry %q in the media section", hdr.Name)
		}
		if target == nil {
			continue
		}
		ct := mime.TypeByExtension(path.Ext(key))
		if ct == "" {
			ct = "application/octet-stream"
		}
		if err := target.Put(ctx, key, tr, hdr.Size, ct); err != nil {
			return m, counts, fmt.Errorf("restoring %s to %s: %w", key, target.Describe(), err)
		}
		counts.MediaFiles++
		counts.MediaBytes += hdr.Size
	}
	if target != nil {
		log.Info("media restored", "files", counts.MediaFiles, "bytes", counts.MediaBytes, "storage", target.Describe())
	}
	return m, counts, nil
}

// prepareTarget makes sure the database is empty, dropping every table when it is not
// and force is set (or when it holds a migrated instance that was never set up).
func prepareTarget(ctx context.Context, pool *pgxpool.Pool, force bool, log *slog.Logger) error {
	var tables []string
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = current_schema() ORDER BY tablename`)
	if err != nil {
		return err
	}
	if tables, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return err
	}
	if len(tables) == 0 {
		return nil
	}
	if !force {
		var configured bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users) OR EXISTS (SELECT 1 FROM instance_settings WHERE setup_completed_at IS NOT NULL)`).Scan(&configured)
		if err != nil {
			return fmt.Errorf("%w (%s)", ErrNotEmpty, err.Error())
		}
		if configured {
			return ErrNotEmpty
		}
	}
	quoted := make([]string, len(tables))
	for i, t := range tables {
		quoted[i] = pgx.Identifier{t}.Sanitize()
	}
	if _, err := pool.Exec(ctx, "DROP TABLE "+strings.Join(quoted, ", ")+" CASCADE"); err != nil {
		return fmt.Errorf("clearing the target database: %w", err)
	}
	log.Info("cleared the target database", "tables", len(tables))
	return nil
}

type foreignKey struct{ table, name, def string }

func dropForeignKeys(ctx context.Context, tx pgx.Tx) ([]foreignKey, error) {
	rows, err := tx.Query(ctx, `
		SELECT c.conrelid::regclass::text, c.conname, pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE c.contype = 'f' AND n.nspname = current_schema()
		ORDER BY 1, 2`)
	if err != nil {
		return nil, err
	}
	fks, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (foreignKey, error) {
		var fk foreignKey
		err := r.Scan(&fk.table, &fk.name, &fk.def)
		fk.table = strings.Trim(fk.table, `"`)
		return fk, err
	})
	if err != nil {
		return nil, err
	}
	for _, fk := range fks {
		if _, err := tx.Exec(ctx, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s",
			pgx.Identifier{fk.table}.Sanitize(), pgx.Identifier{fk.name}.Sanitize())); err != nil {
			return nil, err
		}
	}
	return fks, nil
}
