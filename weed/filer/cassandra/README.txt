See README.md for setup, compatibility, migration boundaries, and tests.

For a new installation, create a keyspace with an appropriate replication
policy and execute schema.cql in that keyspace. New tables use the varchar
dirhash partition key. Existing PRIMARY KEY (directory, name) tables remain
supported without migration; the filer detects the layout at initialization.
It does not create, alter, or migrate tables automatically.
