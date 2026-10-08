package cassandra

import (
	"testing"

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
