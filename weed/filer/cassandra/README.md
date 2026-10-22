# Cassandra filer store

Create a keyspace with replication appropriate for your Cassandra deployment,
then create `filemeta` before starting the filer. For a **new** installation:

```sh
cqlsh <host> -k <keyspace> -f weed/filer/cassandra/schema.cql
```

Configure that keyspace and its hosts under `[cassandra]` in `filer.toml`.
The filer reads schema metadata once during initialization and selects one of
these layouts for all file metadata and KV operations:

| Partition key | Clustering key | Other required columns | Behavior |
| --- | --- | --- | --- |
| `directory varchar` | `name varchar` | `meta blob` | Existing directory-keyed tables remain supported without migration. |
| `dirhash varchar` | `name varchar` | `directory varchar`, `meta blob` | New tables use the full, lowercase 32-character MD5 hex string of the stored directory key. |

`text` is an equivalent alias for `varchar` and is also accepted. `meta`, and `directory` in the
hashed layout, must be regular columns, not static columns. Adding an ordinary
`dirhash` column to a legacy table does **not** switch its layout. Composite or
different primary keys and incompatible column types fail initialization with
an error; the session is closed. A missing table also fails initialization.
There is no automatic CREATE, ALTER, DROP, backfill, or schema lookup on the
request path. Restart the filer after an intentional schema change.

The schema file uses `IF NOT EXISTS`: running it against an existing legacy
table leaves that table unchanged. Old tables do not need a forced migration.
Do not run an old binary that only understands `(directory, name)` against a
new `(dirhash, name)` table. Multiple old/new binaries can use the unchanged
legacy layout; mixed writers do not provide a migration mechanism.

If changing layouts is necessary, treat it as a separate data migration.
Cassandra cannot alter a table's primary key. Back up the source, stop or
otherwise coordinate writers, and reinsert all live rows into a separately
created target keyspace/table with the target layout. Include opaque KV rows
as well as file metadata: store identity, replication offsets, and hard links
use the same table. Compute MD5 from the **stored** `directory` value, including
the existing `superLargeDirectories` and KV key transformations, and preserve
the values and remaining TTL/write-time semantics. Validate data and KV state
before switching the configured keyspace; a raw snapshot restore does not
convert primary keys. No migration or rollback automation is supplied here.

Hashing keeps one ordinary directory in one partition; it is not sharding for
a large directory or a demonstrated performance improvement. The original
`superLargeDirectories` layout and its listing limitations are unchanged.
Directory hashes are used as keys without a collision-resolution scheme.

## Prefixed listing

`ListDirectoryPrefixedEntries` (S3 ListObjects with a prefix, filer listings
with a name prefix) reads one range of the directory's partition in both
layouts: `name >= max(start, prefix) AND name < bound`, where `bound` is the
prefix with its last code point replaced by the next one (trailing U+10FFFF is
dropped first). Cassandra orders text clustering keys by their UTF-8 bytes,
which is code point order for valid UTF-8, so the range holds exactly the
names with the prefix; a byte-wise increment could produce invalid UTF-8,
which Cassandra rejects as a text value. An empty prefix uses the plain
listing. A prefix that is not valid UTF-8 or consists only of U+10FFFF has no
bound; the store returns `ErrUnsupportedListDirectoryPrefixed` and the filer
store wrapper lists from the start name and filters in memory, as it did for
every prefix before. `superLargeDirectories` remain unlistable.

Previously every prefixed listing read the directory from its start (or the
start name) up to the prefix and filtered in memory. With 1,000,000 entries in
one directory and a prefix matching the last 100, one listing read 1,000,000
rows (about 2.1–2.4 s on a single local node) before and 100 rows (4–8 ms)
after; see `TestCassandraPrefixListingLargeDirectory`.

## Tests

Unit tests run without Cassandra:

```sh
go test ./weed/filer/cassandra -run 'TestDetectFileMetaLayout|TestPrefix' -count=1
```

Integration tests are opt-in and must point to a **disposable** Cassandra
instance. They create and drop uniquely named `swfs_test_*` keyspaces, use
replication factor 1, and never use the application's configured keyspace.
The endpoint must be reachable at the address/port advertised by Cassandra.
For a disposable local server listening and advertising `127.0.0.1:9042`:

```sh
SEAWEEDFS_CASSANDRA_TEST_HOSTS=127.0.0.1:9042 \
  go test ./weed/filer/cassandra -run TestCassandraStoreIntegration -count=1 -v -timeout=5m
```

For an instance with password authentication, also set
`SEAWEEDFS_CASSANDRA_TEST_USERNAME` and `SEAWEEDFS_CASSANDRA_TEST_PASSWORD`.

The integration suite executes `schema.cql`, reads pre-existing legacy rows,
exercises both layouts through initialization, CRUD, ordered listing and
inclusive/exclusive cursors, prefixed listing (compared with the wrapper's
in-memory filtering), directory deletion, KV operations, TTL expiration, and
super-large-directory point operations. It also checks missing/unsupported
schemas, session cleanup, and propagation of query and iterator failures.

`TestCassandraPrefixListingLargeDirectory` writes
`SEAWEEDFS_CASSANDRA_LARGE_DIR_ENTRIES` entries (default 1,000,000) into one
directory of the hashed layout and logs the rows read and the time taken by a
prefixed listing at its end, before (in-memory filtering) and after (range
query). It is skipped with `-short`; allow a few minutes for the writes.
