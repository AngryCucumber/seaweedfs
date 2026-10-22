package cassandra

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"github.com/gocql/gocql"
	"github.com/seaweedfs/seaweedfs/weed/util"
)

// A store keeps the layout discovered at initialization. Changing the table's
// layout under a running filer is not supported.
type fileMetaLayout struct {
	partitionColumn     string
	insert              string
	find                string
	deleteEntry         string
	deleteChildren      string
	listExclusive       string
	listInclusive       string
	listPrefixExclusive string
	listPrefixInclusive string
}

func detectFileMetaLayout(table *gocql.TableMetadata) (fileMetaLayout, error) {
	if table == nil {
		return fileMetaLayout{}, fmt.Errorf("filemeta table is missing; create it using weed/filer/cassandra/schema.cql before starting the filer")
	}
	unsupported := func(reason string) (fileMetaLayout, error) {
		return fileMetaLayout{}, fmt.Errorf("unsupported filemeta schema: %s; expected PRIMARY KEY (directory, name) or PRIMARY KEY (dirhash, name), with varchar directory/name/dirhash and blob meta", reason)
	}
	if len(table.PartitionKey) != 1 || table.PartitionKey[0] == nil {
		return unsupported("expected one partition key column")
	}
	partitionColumn := table.PartitionKey[0].Name
	if partitionColumn != "directory" && partitionColumn != "dirhash" {
		return unsupported("unexpected partition key " + partitionColumn)
	}
	if len(table.ClusteringColumns) != 1 || table.ClusteringColumns[0] == nil || table.ClusteringColumns[0].Name != "name" {
		return unsupported("expected name as the only clustering column")
	}
	for _, name := range []string{"directory", "name", partitionColumn} {
		column := table.Columns[name]
		if column == nil || column.Type == nil || (column.Type.Type() != gocql.TypeText && column.Type.Type() != gocql.TypeVarchar) {
			return unsupported(name + " must be varchar or text")
		}
	}
	meta := table.Columns["meta"]
	if meta == nil || meta.Type == nil || meta.Type.Type() != gocql.TypeBlob || meta.Kind != gocql.ColumnRegular {
		return unsupported("meta must be a regular blob column")
	}
	if partitionColumn == "dirhash" && table.Columns["directory"].Kind != gocql.ColumnRegular {
		return unsupported("directory must be a regular varchar column in the dirhash layout")
	}

	layout := fileMetaLayout{
		partitionColumn:     partitionColumn,
		insert:              "INSERT INTO filemeta (directory,name,meta) VALUES(?,?,?) USING TTL ?",
		find:                "SELECT meta FROM filemeta WHERE " + partitionColumn + "=? AND name=?",
		deleteEntry:         "DELETE FROM filemeta WHERE " + partitionColumn + "=? AND name=?",
		deleteChildren:      "DELETE FROM filemeta WHERE " + partitionColumn + "=?",
		listExclusive:       "SELECT name, meta FROM filemeta WHERE " + partitionColumn + "=? AND name>? ORDER BY name ASC LIMIT ?",
		listInclusive:       "SELECT name, meta FROM filemeta WHERE " + partitionColumn + "=? AND name>=? ORDER BY name ASC LIMIT ?",
		listPrefixExclusive: "SELECT name, meta FROM filemeta WHERE " + partitionColumn + "=? AND name>? AND name<? ORDER BY name ASC LIMIT ?",
		listPrefixInclusive: "SELECT name, meta FROM filemeta WHERE " + partitionColumn + "=? AND name>=? AND name<? ORDER BY name ASC LIMIT ?",
	}
	if partitionColumn == "dirhash" {
		layout.insert = "INSERT INTO filemeta (dirhash,directory,name,meta) VALUES(?,?,?,?) USING TTL ?"
	}
	return layout, nil
}

func (layout fileMetaLayout) partitionValue(directory string) string {
	if layout.partitionColumn == "dirhash" {
		return util.Md5String([]byte(directory))
	}
	return directory
}

// prefixListQuery returns the range query of a prefixed listing from
// startFileName: names from max(startFileName, prefix) up to the prefix's
// upper bound. ok is false when the prefix has no upper bound.
func (layout fileMetaLayout) prefixListQuery(startFileName string, includeStartFile bool, prefix string) (cql, lower, upper string, ok bool) {
	if upper, ok = prefixUpperBound(prefix); !ok {
		return "", "", "", false
	}
	lower, inclusive := prefix, true
	if startFileName >= prefix {
		lower, inclusive = startFileName, includeStartFile
	}
	cql = layout.listPrefixExclusive
	if inclusive {
		cql = layout.listPrefixInclusive
	}
	return cql, lower, upper, true
}

// prefixUpperBound returns the smallest valid UTF-8 string that sorts after
// every name starting with prefix. Cassandra orders text clustering keys by
// their UTF-8 bytes, which for valid UTF-8 is code point order, so the bound
// is the prefix with its last code point replaced by the next one; trailing
// U+10FFFF has no next code point and is dropped first. Incrementing the last
// byte instead could yield invalid UTF-8, which Cassandra rejects as a text
// value. ok is false for an empty prefix, a prefix that is not valid UTF-8
// (no stored name is) and a prefix made only of U+10FFFF.
func prefixUpperBound(prefix string) (bound string, ok bool) {
	if prefix == "" || !utf8.ValidString(prefix) {
		return "", false
	}
	for prefix != "" {
		last, size := utf8.DecodeLastRuneInString(prefix)
		prefix = prefix[:len(prefix)-size]
		if last == unicode.MaxRune {
			continue
		}
		next := last + 1
		if next == 0xD800 { // surrogates cannot be encoded in UTF-8
			next = 0xE000
		}
		return prefix + string(next), true
	}
	return "", false
}

func (store *CassandraStore) insertQuery(directory, name string, meta []byte, ttl int32) *gocql.Query {
	if store.layout.partitionColumn == "dirhash" {
		return store.session.Query(store.layout.insert, store.layout.partitionValue(directory), directory, name, meta, ttl)
	}
	return store.session.Query(store.layout.insert, directory, name, meta, ttl)
}
