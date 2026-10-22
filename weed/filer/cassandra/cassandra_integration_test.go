package cassandra

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
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
	hosts := integrationHosts(t)
	username, password := integrationCredentials()
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
			if err := store.initialize(keyspace, hosts, username, password, []string{"/huge"}, "", 10000); err != nil {
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

			prefixDir := util.FullPath("/prefix")
			for _, name := range []string{"a", "p", "p1", "p2", "q", "目录", "目录-1", "目\U0010ffff"} {
				if err := store.InsertEntry(ctx, integrationEntry(string(prefixDir.Child(name)), name)); err != nil {
					t.Fatal(err)
				}
			}
			ranged := filer.NewFilerStoreWrapper(store)
			legacy := filer.NewFilerStoreWrapper(&legacyPrefixStore{CassandraStore: store})
			for _, c := range []struct {
				prefix, start string
				include       bool
				limit         int64
				want          []string
			}{
				{prefix: "p", limit: 10, want: []string{"p", "p1", "p2"}},
				{prefix: "p", start: "p", limit: 10, want: []string{"p1", "p2"}},
				{prefix: "p", start: "p1", include: true, limit: 10, want: []string{"p1", "p2"}},
				{prefix: "p", start: "a", limit: 10, want: []string{"p", "p1", "p2"}},
				{prefix: "p", limit: 2, want: []string{"p", "p1"}},
				{prefix: "目录", limit: 10, want: []string{"目录", "目录-1"}},
				{prefix: "目", limit: 10, want: []string{"目录", "目录-1", "目\U0010ffff"}},
				{prefix: "x", limit: 10},
				{prefix: "p", start: "z", limit: 10},
				{prefix: "\U0010ffff", limit: 10}, // no upper bound: the wrapper lists and filters
				{prefix: "", start: "p2", limit: 10, want: []string{"q", "目录", "目录-1", "目\U0010ffff"}},
			} {
				for kind, wrapper := range map[string]*filer.FilerStoreWrapper{"range": ranged, "legacy": legacy} {
					names, err := listNames(ctx, wrapper, prefixDir, c.start, c.include, c.limit, c.prefix)
					if err != nil || !reflect.DeepEqual(names, c.want) {
						t.Fatalf("%s prefix=%q start=%q inclusive=%v limit=%d: names=%q err=%v; want %q",
							kind, c.prefix, c.start, c.include, c.limit, names, err, c.want)
					}
				}
			}
			if _, err := store.ListDirectoryPrefixedEntries(ctx, prefixDir, "", false, 10, "\U0010ffff", func(*filer.Entry) bool { return true }); err != filer.ErrUnsupportedListDirectoryPrefixed {
				t.Fatalf("prefix without an upper bound: %v", err)
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
			if names, err := listNames(ctx, filer.NewFilerStoreWrapper(store), "/huge", "", false, 10, "i"); err != nil || len(names) != 0 {
				t.Fatalf("super large directory must stay unlistable with a prefix: %q %v", names, err)
			}
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
			if _, err := store.ListDirectoryPrefixedEntries(ctx, "/seed", "", false, 10, "e", func(*filer.Entry) bool { return true }); err == nil {
				t.Fatal("prefixed list swallowed query/iterator failure")
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
			err := store.initialize(keyspace, hosts, username, password, nil, "", 10000)
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

// TestCassandraPrefixListingLargeDirectory lists a prefix at the end of one
// large directory the way the filer did before the store implemented
// ListDirectoryPrefixedEntries and the way it does now, and logs the rows
// each read and the time taken. It writes SEAWEEDFS_CASSANDRA_LARGE_DIR_ENTRIES
// entries (default 1000000) into the dirhash layout.
func TestCassandraPrefixListingLargeDirectory(t *testing.T) {
	hosts := integrationHosts(t)
	username, password := integrationCredentials()
	if testing.Short() {
		t.Skip("writes a large directory")
	}
	total := 1000000
	if value := os.Getenv("SEAWEEDFS_CASSANDRA_LARGE_DIR_ENTRIES"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			t.Fatalf("SEAWEEDFS_CASSANDRA_LARGE_DIR_ENTRIES=%q", value)
		}
		total = n
	}
	admin := integrationSession(t, hosts, "")
	keyspace := integrationKeyspace(t, admin)
	seed := integrationSession(t, hosts, keyspace)
	schema, err := os.ReadFile("schema.cql")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Query(string(schema)).Exec(); err != nil {
		t.Fatal(err)
	}

	const dir = "/buckets/large"
	meta, err := integrationEntry(dir+"/object", "small object").EncodeAttributesAndChunks()
	if err != nil {
		t.Fatal(err)
	}
	dirHash := fmt.Sprintf("%x", md5.Sum([]byte(dir)))
	name := func(i int) string { return fmt.Sprintf("obj-%07d", i) }
	started := time.Now()
	work := make(chan int, 1024)
	var failed sync.Once
	var writeErr error
	var wg sync.WaitGroup
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := seed.Query("INSERT INTO filemeta (dirhash,directory,name,meta) VALUES (?,?,?,?)", dirHash, dir, name(i), meta).Exec(); err != nil {
					failed.Do(func() { writeErr = err })
				}
			}
		}()
	}
	for i := 0; i < total; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	if writeErr != nil {
		t.Fatalf("write entries: %v", writeErr)
	}
	t.Logf("wrote %d entries into %s in %v", total, dir, time.Since(started).Round(time.Millisecond))

	store := &CassandraStore{}
	if err := store.initialize(keyspace, hosts, username, password, nil, "", 10000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Shutdown)
	last := name(total - 1)
	prefix := last[:len(last)-2]
	var want []string
	for i := 0; i < total; i++ {
		if strings.HasPrefix(name(i), prefix) {
			want = append(want, name(i))
		}
	}

	ctx := context.Background()
	const limit = 1002 // what S3 asks the filer for one ListObjects page of 1000 keys
	legacyStore := &legacyPrefixStore{CassandraStore: store}
	started = time.Now()
	before, err := listNames(ctx, filer.NewFilerStoreWrapper(legacyStore), dir, "", false, limit, prefix)
	beforeTime := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	started = time.Now()
	after, err := listNames(ctx, filer.NewFilerStoreWrapper(store), dir, "", false, limit, prefix)
	afterTime := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, want) || !reflect.DeepEqual(after, want) {
		t.Fatalf("prefix %q: before %d names, after %d names, want %d", prefix, len(before), len(after), len(want))
	}
	t.Logf("prefix %q at the end of %d entries, %d matches: before %d rows read in %v, after %d rows read in %v",
		prefix, total, len(want), legacyStore.rows, beforeTime.Round(time.Millisecond), len(after), afterTime.Round(time.Millisecond))
}

// legacyPrefixStore lists prefixes the way the filer did before the store
// implemented ListDirectoryPrefixedEntries and counts the rows it reads.
type legacyPrefixStore struct {
	*CassandraStore
	rows int
}

func (store *legacyPrefixStore) ListDirectoryPrefixedEntries(context.Context, util.FullPath, string, bool, int64, string, filer.ListEachEntryFunc) (string, error) {
	return "", filer.ErrUnsupportedListDirectoryPrefixed
}

func (store *legacyPrefixStore) ListDirectoryEntries(ctx context.Context, dirPath util.FullPath, startFileName string, includeStartFile bool, limit int64, eachEntryFunc filer.ListEachEntryFunc) (string, error) {
	return store.CassandraStore.ListDirectoryEntries(ctx, dirPath, startFileName, includeStartFile, limit, func(entry *filer.Entry) bool {
		store.rows++
		return eachEntryFunc(entry)
	})
}

// listNames pages through a prefixed listing the way the filer's ListEntries
// does: it lists again from the last name until a page is empty or limit
// names were returned.
func listNames(ctx context.Context, store *filer.FilerStoreWrapper, dir util.FullPath, start string, include bool, limit int64, prefix string) (names []string, err error) {
	page := min(limit, int64(filer.PaginationSize))
	for limit > 0 {
		found := false
		start, err = store.ListDirectoryPrefixedEntries(ctx, dir, start, include, page, prefix, func(entry *filer.Entry) bool {
			found = true
			names = append(names, entry.FullPath.Name())
			limit--
			return limit > 0
		})
		if err != nil || !found {
			return names, err
		}
		include = false
	}
	return names, nil
}

func integrationHosts(t *testing.T) []string {
	t.Helper()
	hostsValue := os.Getenv("SEAWEEDFS_CASSANDRA_TEST_HOSTS")
	if hostsValue == "" {
		t.Skip("set SEAWEEDFS_CASSANDRA_TEST_HOSTS to a disposable Cassandra instance")
	}
	hosts := strings.Split(hostsValue, ",")
	for i := range hosts {
		hosts[i] = strings.TrimSpace(hosts[i])
	}
	return hosts
}

// integrationCredentials are SEAWEEDFS_CASSANDRA_TEST_USERNAME and
// SEAWEEDFS_CASSANDRA_TEST_PASSWORD, for an instance with password authentication.
func integrationCredentials() (username, password string) {
	return os.Getenv("SEAWEEDFS_CASSANDRA_TEST_USERNAME"), os.Getenv("SEAWEEDFS_CASSANDRA_TEST_PASSWORD")
}

func integrationSession(t *testing.T, hosts []string, keyspace string) *gocql.Session {
	t.Helper()
	cluster := gocql.NewCluster(hosts...)
	if username, password := integrationCredentials(); username != "" {
		cluster.Authenticator = gocql.PasswordAuthenticator{Username: username, Password: password}
	}
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
