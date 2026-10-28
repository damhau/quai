// Package pgtest gives tests a real PostgreSQL (docs/engineering/testing.md): the server in
// QUAI_TEST_DATABASE_URL, or a throwaway PostgreSQL 17 started with embedded-postgres, listening
// on TCP only so that it works in sandboxes without Unix sockets. Its binaries are downloaded once
// into the user cache directory. Each test gets its own database, created from a migrated
// template and dropped when the test ends.
//
// A package that uses it calls Main from TestMain.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/damhau/quai/backend/internal/store"
)

const template = "quai_template"

var admin string // URL of the server's maintenance database

// Main starts PostgreSQL, prepares the migrated template database, runs the tests and stops
// PostgreSQL. Use it as `func TestMain(m *testing.M) { os.Exit(pgtest.Main(m)) }`.
func Main(m *testing.M) int {
	return Around(m.Run)()
}

// Around returns run wrapped in PostgreSQL's start and stop, for a package whose TestMain also
// starts something else: `os.Exit(giteatest.Main(m))` becomes
// `os.Exit(pgtest.Around(func() int { return giteatest.Main(m) })())`.
func Around(run func() int) func() int {
	return func() int {
		stop, err := start()
		if err != nil {
			fmt.Fprintln(os.Stderr, "pgtest:", err)
			return 1
		}
		code := run()
		if err := stop(); err != nil {
			fmt.Fprintln(os.Stderr, "pgtest: stop:", err)
		}
		return code
	}
}

func start() (stop func() error, err error) {
	if u := os.Getenv("QUAI_TEST_DATABASE_URL"); u != "" {
		admin = u
		return func() error { return nil }, prepareTemplate()
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	port := uint32(l.Addr().(*net.TCPAddr).Port) // #nosec G115 -- a TCP port fits
	_ = l.Close()
	dir, err := os.MkdirTemp("", "quai-pgtest-")
	if err != nil {
		return nil, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	var log io.Writer = io.Discard
	if os.Getenv("QUAI_TEST_VERBOSE") != "" {
		log = os.Stderr
	}
	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Version(embeddedpostgres.V17).
		Port(port).
		Username("quai").Password("quai").Database("quai").
		RuntimePath(filepath.Join(dir, "run")).
		DataPath(filepath.Join(dir, "data")).
		CachePath(filepath.Join(cache, "quai", "embedded-postgres")).
		StartTimeout(2 * time.Minute).
		StartParameters(map[string]string{
			"unix_socket_directories": "",
			// Durability is not needed for a database that is thrown away.
			"fsync": "off", "synchronous_commit": "off", "full_page_writes": "off",
		}).
		Logger(log))
	if err := pg.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("start embedded PostgreSQL: %w", err)
	}
	admin = fmt.Sprintf("postgres://quai:quai@127.0.0.1:%d/quai?sslmode=disable", port)
	stop = func() error {
		err := pg.Stop()
		_ = os.RemoveAll(dir)
		return err
	}
	if err := prepareTemplate(); err != nil {
		_ = stop()
		return nil, err
	}
	return stop, nil
}

// prepareTemplate creates the template database, migrated, unless it exists.
func prepareTemplate() error {
	ctx := context.Background()
	a, err := pxpool(ctx, admin)
	if err != nil {
		return err
	}
	defer a.Close()
	var exists bool
	if err := a.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", template).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		if _, err := a.Exec(ctx, "CREATE DATABASE "+template); err != nil {
			return err
		}
	}
	t, err := pxpool(ctx, withDatabase(admin, template))
	if err != nil {
		return err
	}
	defer t.Close()
	_, err = store.Migrate(ctx, t)
	return err
}

func pxpool(ctx context.Context, u string) (*pgxpool.Pool, error) {
	p, err := pgxpool.New(ctx, u)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return p, nil
}

func withDatabase(u, name string) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return u
	}
	parsed.Path = "/" + name
	return parsed.String()
}

// URL returns the URL of a new, migrated database for the test, dropped when the test ends.
func URL(t testing.TB) string {
	t.Helper()
	if admin == "" {
		t.Fatal("pgtest: call pgtest.Main from TestMain")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "t_" + hex.EncodeToString(b)
	a, err := pxpool(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+template); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := pxpool(ctx, admin)
		if err != nil {
			t.Error(err)
			return
		}
		defer a.Close()
		if _, err := a.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Error(err)
		}
	})
	return withDatabase(admin, name)
}

// Store returns a store on a new, migrated database for the test.
func Store(t testing.TB) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), URL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}
