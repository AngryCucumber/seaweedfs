package cassandra

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gocql/gocql"
)

func testFileMetaSchema(partition string) *gocql.TableMetadata {
	column := func(name string, kind gocql.ColumnKind, typ gocql.Type) *gocql.ColumnMetadata {
		return &gocql.ColumnMetadata{Name: name, Kind: kind, Type: gocql.NewNativeType(4, typ, "")}
	}
	columns := map[string]*gocql.ColumnMetadata{
		"directory": column("directory", gocql.ColumnRegular, gocql.TypeVarchar),
		"dirhash":   column("dirhash", gocql.ColumnRegular, gocql.TypeText),
		"name":      column("name", gocql.ColumnClusteringKey, gocql.TypeVarchar),
		"meta":      column("meta", gocql.ColumnRegular, gocql.TypeBlob),
	}
	columns[partition].Kind = gocql.ColumnPartitionKey
	return &gocql.TableMetadata{
		Name:              "filemeta",
		Columns:           columns,
		PartitionKey:      []*gocql.ColumnMetadata{columns[partition]},
		ClusteringColumns: []*gocql.ColumnMetadata{columns["name"]},
	}
}

func TestDetectFileMetaLayout(t *testing.T) {
	tests := []struct {
		name      string
		partition string
		change    func(*gocql.TableMetadata)
		wantError bool
	}{
		{name: "legacy without hash", partition: "directory", change: func(table *gocql.TableMetadata) { delete(table.Columns, "dirhash") }},
		{name: "legacy with unused hash column", partition: "directory"},
		{name: "hashed", partition: "dirhash"},
		{name: "composite partition", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) {
			table.PartitionKey = append(table.PartitionKey, table.Columns["directory"])
		}},
		{name: "hash is clustering instead of partition", partition: "directory", wantError: true, change: func(table *gocql.TableMetadata) {
			table.ClusteringColumns = append([]*gocql.ColumnMetadata{table.Columns["dirhash"]}, table.ClusteringColumns...)
		}},
		{name: "missing clustering key", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) { table.ClusteringColumns = nil }},
		{name: "hash bigint", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) {
			table.Columns["dirhash"].Type = gocql.NewNativeType(4, gocql.TypeBigInt, "")
		}},
		{name: "missing directory", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) { delete(table.Columns, "directory") }},
		{name: "missing meta", partition: "directory", wantError: true, change: func(table *gocql.TableMetadata) { delete(table.Columns, "meta") }},
		{name: "text meta", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) {
			table.Columns["meta"].Type = gocql.NewNativeType(4, gocql.TypeText, "")
		}},
		{name: "static meta", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) { table.Columns["meta"].Kind = gocql.ColumnStatic }},
		{name: "static directory", partition: "dirhash", wantError: true, change: func(table *gocql.TableMetadata) { table.Columns["directory"].Kind = gocql.ColumnStatic }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			table := testFileMetaSchema(tt.partition)
			if tt.change != nil {
				tt.change(table)
			}
			layout, err := detectFileMetaLayout(table)
			if (err != nil) != tt.wantError {
				t.Fatalf("detectFileMetaLayout error = %v, want error %v", err, tt.wantError)
			}
			if !tt.wantError && layout.partitionColumn != tt.partition {
				t.Fatalf("partition = %q, want %q", layout.partitionColumn, tt.partition)
			}
		})
	}
	if _, err := detectFileMetaLayout(nil); err == nil {
		t.Fatal("missing filemeta table was accepted")
	}
}

func TestPrefixUpperBound(t *testing.T) {
	tests := []struct {
		prefix string
		want   string
		ok     bool
	}{
		{prefix: "", ok: false},
		{prefix: "abc", want: "abd", ok: true},
		{prefix: "photos/", want: "photos0", ok: true},
		{prefix: "a\x7f", want: "a\u0080", ok: true},       // one byte grows to two
		{prefix: "a\u07ff", want: "a\u0800", ok: true},     // two bytes grow to three
		{prefix: "目录", want: "目\u5f56", ok: true},          // 录 is U+5F55
		{prefix: "a\ud7ff", want: "a\ue000", ok: true},     // skips the surrogates
		{prefix: "a\uffff", want: "a\U00010000", ok: true}, // three bytes grow to four
		{prefix: "a\U0010ffff", want: "b", ok: true},       // no code point after U+10FFFF
		{prefix: "目\U0010ffff\U0010ffff", want: "\u76ef", ok: true},
		{prefix: "\U0010ffff", ok: false},
		{prefix: "\U0010ffff\U0010ffff", ok: false},
		{prefix: "ab\xff", ok: false},     // ends with 0xFF: not UTF-8, no stored name has it
		{prefix: "ab\xe4\xb8", ok: false}, // truncated multi-byte character
		{prefix: "\xffab", ok: false},
	}
	for _, tt := range tests {
		got, ok := prefixUpperBound(tt.prefix)
		if ok != tt.ok || got != tt.want {
			t.Errorf("prefixUpperBound(%q) = %q, %v; want %q, %v", tt.prefix, got, ok, tt.want, tt.ok)
		}
		if ok && !utf8.ValidString(got) {
			t.Errorf("prefixUpperBound(%q) = %q is not valid UTF-8", tt.prefix, got)
		}
	}
}

// The range [prefix, bound) must hold exactly the names that start with the
// prefix, under the byte order Cassandra uses for text clustering keys.
func TestPrefixUpperBoundMatchesHasPrefix(t *testing.T) {
	alphabet := []rune{'a', 'b', '/', 0x7f, 0x80, 0x7ff, 0x800, '目', '录', 0xd7ff, 0xe000, 0xffff, 0x10000, 0x10ffff}
	random := rand.New(rand.NewSource(1))
	word := func(maxLen int) string {
		var b strings.Builder
		for n := random.Intn(maxLen + 1); n > 0; n-- {
			b.WriteRune(alphabet[random.Intn(len(alphabet))])
		}
		return b.String()
	}
	names := make([]string, 3000)
	for i := range names {
		names[i] = word(5)
	}
	sort.Strings(names)
	for i := 0; i < 500; i++ {
		prefix := word(3)
		bound, ok := prefixUpperBound(prefix)
		if !ok {
			if prefix != "" && strings.Trim(prefix, "\U0010ffff") != "" {
				t.Fatalf("no bound for %q", prefix)
			}
			continue
		}
		for _, name := range names {
			inRange := name >= prefix && name < bound
			if inRange != strings.HasPrefix(name, prefix) {
				t.Fatalf("prefix %q bound %q: name %q in range %v, has prefix %v", prefix, bound, name, inRange, !inRange)
			}
		}
	}
}

func TestPrefixListQuery(t *testing.T) {
	for _, partition := range []string{"directory", "dirhash"} {
		layout, err := detectFileMetaLayout(testFileMetaSchema(partition))
		if err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{layout.listPrefixExclusive, layout.listPrefixInclusive} {
			if !strings.Contains(query, "WHERE "+partition+"=? AND name>") || !strings.Contains(query, "AND name<? ORDER BY name ASC LIMIT ?") {
				t.Fatalf("%s: unexpected prefix query %q", partition, query)
			}
		}
		tests := []struct {
			start   string
			include bool
			lower   string
			cql     string
		}{
			{start: "", include: false, lower: "p", cql: layout.listPrefixInclusive},
			{start: "a", include: false, lower: "p", cql: layout.listPrefixInclusive},
			{start: "p", include: false, lower: "p", cql: layout.listPrefixExclusive},
			{start: "p", include: true, lower: "p", cql: layout.listPrefixInclusive},
			{start: "p1", include: false, lower: "p1", cql: layout.listPrefixExclusive},
			{start: "p1", include: true, lower: "p1", cql: layout.listPrefixInclusive},
			{start: "z", include: false, lower: "z", cql: layout.listPrefixExclusive}, // past the bound: the store reads nothing
		}
		for _, tt := range tests {
			cql, lower, upper, ok := layout.prefixListQuery(tt.start, tt.include, "p")
			if !ok || cql != tt.cql || lower != tt.lower || upper != "q" {
				t.Errorf("%s start=%q include=%v: got %q %q %q %v", partition, tt.start, tt.include, cql, lower, upper, ok)
			}
		}
		if _, _, _, ok := layout.prefixListQuery("", false, "\U0010ffff"); ok {
			t.Errorf("%s: prefix without an upper bound must fall back", partition)
		}
	}
}
