1. create a keyspace

CREATE KEYSPACE seaweedfs WITH replication = {'class':'SimpleStrategy', 'replication_factor' : 1};

2. create filemeta table

 USE seaweedfs;

 CREATE TABLE filemeta (
    dirhash varchar,
    directory varchar,
    name varchar,
    meta blob,
    PRIMARY KEY (dirhash, name)
 ) WITH CLUSTERING ORDER BY (name ASC);

Note: `dirhash` is the MD5 hex string of `directory` and is the partition key.
It is computed by the filer at write time (see util.Md5String in
weed/filer/cassandra/cassandra_store.go) so that hot directories spread evenly
across Cassandra nodes. Queries never filter on `directory` directly.

3. migrating from the pre-dirhash schema

Older deployments used `PRIMARY KEY (directory, name)`. Cassandra cannot change
a partition key in place, so migrate by creating a new table and copying:

 CREATE TABLE filemeta_new (
    dirhash varchar,
    directory varchar,
    name varchar,
    meta blob,
    PRIMARY KEY (dirhash, name)
 ) WITH CLUSTERING ORDER BY (name ASC);

 -- copy with a Spark/CQL ETL job, computing dirhash = lower(hex(md5(directory)))
 -- e.g. SELECT directory, name, meta FROM filemeta;
 --      INSERT INTO filemeta_new (dirhash, directory, name, meta)
 --      VALUES (<md5(directory)>, directory, name, meta);

 DROP TABLE filemeta;
 ALTER TABLE filemeta_new RENAME TO filemeta;
