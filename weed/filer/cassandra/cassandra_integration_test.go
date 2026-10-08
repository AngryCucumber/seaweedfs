package cassandra

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gocql/gocql"
	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

const legacyTestSchema = `CREATE TABLE filemeta (
	directory varchar, name varchar, meta blob,
	PRIMARY KEY (directory, name)
) WITH CLUSTERING ORDER BY (name ASC)`

func TestCassandraStoreIntegration(t *testing.T) {
	hostsValue := os.Getenv("SEAWEEDFS_CASSANDRA_TEST_HOSTS")
	if hostsValue == "" {
		t.Skip("set SEAWEEDFS_CASSANDRA_TEST_HOSTS to a disposable Cassandra instance")
	}
	hosts := strings.Split(hostsValue, ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}
	admin := integrationSession(t, hosts, "")
	hashedSchema, err := os.ReadFile("schema.cql")
	if err != nil {
		t.Fatal(err)
	}

	for _, layout := range []string{"legacy", "legacy_with_hash_column", "dirhash"} {
		t.Run(layout, func(t *testing.T) {
			keyspace := integrationKeyspace(t, admin)
			seed := integrationSession(t, hosts, keyspace)
			ddl := legacyTestSchema
			if layout == "dirhash" {
				ddl = string(hashedSchema)
			}
			if err := seed.Query(ddl).Exec(); err != nil {
				t.Fatalf("create schema: %v", err)
			}
			if layout == "legacy_with_hash_column" {
				if err := seed.Query("ALTER TABLE filemeta ADD dirhash varchar").Exec(); err != nil {
					t.Fatal(err)
				}
			}

			// Seed directly with the old or colleague's key format before this
			// implementation initializes: a write/read round trip alone would
			// not establish compatibility with pre-existing rows.
			entry := integrationEntry("/seed/existing", "pre-existing content")
			encoded, err := entry.EncodeAttributesAndChunks()
			if err != nil {
				t.Fatal(err)
			}
			seedValue := func(directory, name string, value []byte, ttl int32) {
				t.Helper()
				var query *gocql.Query
				if layout == "dirhash" {
					query = seed.Query("INSERT INTO filemeta (dirhash,directory,name,meta) VALUES (?,?,?,?) USING TTL ?",
						fmt.Sprintf("%x", md5.Sum([]byte(directory))), directory, name, value, ttl)
				} else {
					query = seed.Query("INSERT INTO filemeta (directory,name,meta) VALUES (?,?,?) USING TTL ?", directory, name, value, ttl)
				}
				if err := query.Exec(); err != nil {
					t.Fatalf("seed pre-existing row: %v", err)
				}
			}
			seedValue("/seed", "existing", encoded, 0)
			// Independent, fixed encoding of the existing filer.store.id KV key.
			seedValue("ZmlsZXIuc3Q=", "b3JlLmlk", []byte{1, 2, 3, 4}, 0)

			store := &CassandraStore{}
			if err := store.initialize(keyspace, hosts, "", "", []string{"/huge"}, "", 10000); err != nil {
				t.Fatalf("initialize: %v", err)
			}
			t.Cleanup(store.Shutdown)
			wantPartition := "directory"
			if layout == "dirhash" {
				wantPartition = "dirhash"
			}
			if store.layout.partitionColumn != wantPartition {
				t.Fatalf("selected %s, want %s", store.layout.partitionColumn, wantPartition)
			}
			assertIntegrationEntry(t, store, entry.FullPath, entry.Content)
			value, err := store.KvGet(context.Background(), []byte("filer.store.id"))
			if err != nil || !bytes.Equal(value, []byte{1, 2, 3, 4}) {
				t.Fatalf("pre-existing KV = %v, %v", value, err)
			}

			ctx := context.Background()
			for _, name := range []string{"a", "b", "c"} {
				if err := store.InsertEntry(ctx, integrationEntry("/目录/"+name, "content-"+name)); err != nil {
					t.Fatalf("insert %s: %v", name, err)
				}
			}
			if err := store.InsertEntry(ctx, integrationEntry("/other/a", "other directory")); err != nil {
				t.Fatal(err)
			}
			updated := integrationEntry("/目录/b", "updated content")
			if err := store.UpdateEntry(ctx, updated); err != nil {
				t.Fatal(err)
			}
			assertIntegrationEntry(t, store, updated.FullPath, updated.Content)
			for _, cursor := range []struct {
				start   string
				include bool
				want    []string
			}{{"", false, []string{"a", "b", "c"}}, {"b", true, []string{"b", "c"}}, {"b", false, []string{"c"}}} {
				var names []string
				last, err := store.ListDirectoryEntries(ctx, "/目录", cursor.start, cursor.include, 10, func(entry *filer.Entry) bool {
					names = append(names, entry.FullPath.Name())
					return true
				})
				if err != nil || !reflect.DeepEqual(names, cursor.want) || last != "c" {
					t.Fatalf("list start=%q inclusive=%v: names=%v last=%q err=%v", cursor.start, cursor.include, names, last, err)
				}
			}
			last, err := store.ListDirectoryEntries(ctx, "/目录", "", false, 1, func(entry *filer.Entry) bool { return false })
			if err != nil || last != "a" {
				t.Fatalf("stop after first entry: last=%q err=%v", last, err)
			}
			if err := store.DeleteEntry(ctx, "/目录/b"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FindEntry(ctx, "/目录/b"); !errors.Is(err, filer_pb.ErrNotFound) {
				t.Fatalf("deleted entry: %v", err)
			}
			if err := store.DeleteFolderChildren(ctx, "/目录"); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FindEntry(ctx, "/目录/a"); !errors.Is(err, filer_pb.ErrNotFound) {
				t.Fatalf("deleted directory child: %v", err)
			}
			assertIntegrationEntry(t, store, "/other/a", []byte("other directory"))

			for _, key := range [][]byte{[]byte("kv"), []byte("long-key-with-binary\x00\xff")} {
				for _, value := range [][]byte{[]byte("first"), []byte("replacement\x00\xff")} {
					if err := store.KvPut(ctx, key, value); err != nil {
						t.Fatalf("KV insert: %v", err)
					}
					got, err := store.KvGet(ctx, key)
					if err != nil || !bytes.Equal(got, value) {
						t.Fatalf("KV read = %q, %v; want %q", got, err, value)
					}
				}
				if err := store.KvDelete(ctx, key); err != nil {
					t.Fatal(err)
				}
				if _, err := store.KvGet(ctx, key); !errors.Is(err, filer.ErrKvNotFound) {
					t.Fatalf("deleted KV: %v", err)
				}
			}
			if err := store.KvPut(ctx, []byte("empty"), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := store.KvGet(ctx, []byte("empty")); !errors.Is(err, filer.ErrKvNotFound) {
				t.Fatalf("empty KV must retain existing not-found behavior: %v", err)
			}

			huge := integrationEntry("/huge/item", "huge directory entry")
			if err := store.InsertEntry(ctx, huge); err != nil {
				t.Fatal(err)
			}
			assertIntegrationEntry(t, store, huge.FullPath, huge.Content)
			huge.Content = []byte("updated huge entry")
			if err := store.UpdateEntry(ctx, huge); err != nil {
				t.Fatal(err)
			}
			assertIntegrationEntry(t, store, huge.FullPath, huge.Content)
			if err := store.DeleteEntry(ctx, huge.FullPath); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FindEntry(ctx, huge.FullPath); !errors.Is(err, filer_pb.ErrNotFound) {
				t.Fatalf("deleted huge entry: %v", err)
			}

			expiring := integrationEntry("/ttl/entry", "expires")
			expiring.TtlSec = 2
			if err := store.InsertEntry(ctx, expiring); err != nil {
				t.Fatal(err)
			}
			assertIntegrationEntry(t, store, expiring.FullPath, expiring.Content)
			deadline := time.Now().Add(10 * time.Second)
			for {
				_, err := store.FindEntry(ctx, expiring.FullPath)
				if errors.Is(err, filer_pb.ErrNotFound) {
					break
				}
				if err != nil || time.Now().After(deadline) {
					t.Fatalf("TTL did not expire cleanly: %v", err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			assertIntegrationEntry(t, store, "/seed/existing", entry.Content)

			seedValue("/corrupt", "entry", []byte{0xff}, 0)
			if _, err := store.ListDirectoryEntries(ctx, "/corrupt", "", false, 10, func(*filer.Entry) bool { return true }); err == nil {
				t.Fatal("list swallowed metadata decode failure")
			}
			// A real CQL query error must not become a missing key or empty list.
			if err := seed.Query("DROP TABLE filemeta").Exec(); err != nil {
				t.Fatal(err)
			}
			if _, err := store.KvGet(ctx, []byte("filer.store.id")); err == nil || errors.Is(err, filer.ErrKvNotFound) {
				t.Fatalf("KV schema error was lost: %v", err)
			}
			if _, err := store.ListDirectoryEntries(ctx, "/seed", "", false, 10, func(*filer.Entry) bool { return true }); err == nil {
				t.Fatal("list swallowed query/iterator failure")
			}
		})
	}

	for _, bad := range []struct{ name, ddl string }{
		{"missing_table", ""},
		{"wrong_partition", "CREATE TABLE filemeta (dirhash varchar, directory varchar, name varchar, meta blob, PRIMARY KEY ((dirhash, directory), name))"},
		{"wrong_hash_type", "CREATE TABLE filemeta (dirhash bigint, directory varchar, name varchar, meta blob, PRIMARY KEY (dirhash, name))"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			keyspace := integrationKeyspace(t, admin)
			seed := integrationSession(t, hosts, keyspace)
			if bad.ddl != "" {
				if err := seed.Query(bad.ddl).Exec(); err != nil {
					t.Fatal(err)
				}
			}
			store := &CassandraStore{}
			err := store.initialize(keyspace, hosts, "", "", nil, "", 10000)
			if err == nil || !strings.Contains(err.Error(), "filemeta") {
				t.Fatalf("expected explicit schema failure, got %v", err)
			}
			if store.session == nil || !store.session.Closed() {
				t.Fatal("failed initialization did not close its session")
			}
			var count int
			if err := seed.Query("SELECT count(*) FROM system_schema.tables WHERE keyspace_name=? AND table_name=?", keyspace, "filemeta").Scan(&count); err != nil {
				t.Fatal(err)
			}
			wantCount := 1
			if bad.ddl == "" {
				wantCount = 0
			}
			if count != wantCount {
				t.Fatalf("initialization changed table existence: count=%d want=%d", count, wantCount)
			}
		})
	}
}

func integrationSession(t *testing.T, hosts []string, keyspace string) *gocql.Session {
	t.Helper()
	cluster := gocql.NewCluster(hosts...)
	cluster.Keyspace = keyspace
	cluster.Timeout = 10 * time.Second
	cluster.ConnectTimeout = 10 * time.Second
	cluster.Consistency = gocql.LocalQuorum
	session, err := cluster.CreateSession()
	if err != nil {
		t.Fatalf("connect to disposable Cassandra: %v", err)
	}
	t.Cleanup(session.Close)
	return session
}

func integrationKeyspace(t *testing.T, admin *gocql.Session) string {
	t.Helper()
	name := fmt.Sprintf("swfs_test_%d", time.Now().UnixNano())
	if err := admin.Query("CREATE KEYSPACE " + name + " WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}").Exec(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Query("DROP KEYSPACE " + name).Exec(); err != nil {
			t.Errorf("drop test keyspace %s: %v", name, err)
		}
	})
	return name
}

func integrationEntry(path, content string) *filer.Entry {
	return &filer.Entry{
		FullPath: util.FullPath(path),
		Attr:     filer.Attr{Mtime: time.Now(), Crtime: time.Now(), Mode: 0644, FileSize: uint64(len(content))},
		Content:  []byte(content),
	}
}

func assertIntegrationEntry(t *testing.T, store *CassandraStore, path util.FullPath, content []byte) {
	t.Helper()
	entry, err := store.FindEntry(context.Background(), path)
	if err != nil {
		t.Fatalf("find %s: %v", path, err)
	}
	if entry.FullPath != path || !bytes.Equal(entry.Content, content) {
		t.Fatalf("find %s = path %s, content %q; want %q", path, entry.FullPath, entry.Content, content)
	}
}
