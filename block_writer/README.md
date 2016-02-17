# Block writer example

## Summary

The block writer example program is a write-only workload intended to insert
a large amount of data into cockroach quickly. This example is intended to
trigger range splits and rebalances.

## Running

Run against an existing cockroach node or cluster.

#### Development node
```
# Build cockroach binary from https://github.com/cockroachdb/cockroach
# Start it in insecure mode (listens on localhost:26257)
./cockroach start --insecure

# Build block_writer example.
# Start it with:
./block_writer http://localhost:26257
```

#### Insecure node or cluster
```
# Launch your node or cluster in insecure mode (with --insecure passed to cockroach).
# Find a reachable address: [mycockroach:26257].
# Run the example with:
./block_writer http://mycockroach:26257
```

#### Secure node or cluster
```
# Launch your node or cluster in secure mode with certificates in [mycertsdir]
# Find a reachable address:[mycockroach:26257].
# Run the example with:
./block_writer https://mycockroach:26257/?certs=mycertsdir
```
