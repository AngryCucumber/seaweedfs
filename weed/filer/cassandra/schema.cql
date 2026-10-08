-- Run in the keyspace configured in filer.toml. This is for new installations;
-- it does not migrate or change an existing filemeta table.
CREATE TABLE IF NOT EXISTS filemeta (
    dirhash varchar,
    directory varchar,
    name varchar,
    meta blob,
    PRIMARY KEY (dirhash, name)
) WITH CLUSTERING ORDER BY (name ASC);
