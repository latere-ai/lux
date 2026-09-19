# Key write fences

`Store.KeyFences().Put` permanently closes a Key name to creation, rotation and
policy changes. The assertion supplies the name, owner and exact labels; the store
checks an existing occupant and rejects mismatches. A repeat with the same
assertion returns the original fence. The returned insertion flag is true only for
the first installation, so a caller can append its audit event in the same
transaction without duplicating it on replay. `Get` reads that assertion after an uncertain
write outcome. Neither operation exposes a credential.

Installation waits for earlier Key writes to commit. After it succeeds, an older
request cannot commit a new credential or re-enable that name. Deleting and
pruning the Key does not remove the fence. A fenced Key can still be read, deleted
or updated to `disabled: true` while preserving all other metadata, policy and
credential identity. Observation updates remain available.

This primitive does not disable a currently enabled Key. A controller must first
close admission, install the fence, read and disable the current Key with an exact
version precondition, verify the result, and wait for every serving cache's maximum
lifetime before declaring inference revocation complete. Already admitted requests
are a separate lifecycle. HTTP authorization and reconciliation belong to consumers
of this storage interface.

Postgres uses a transaction advisory lock shared by Key writes, deletion and fence
installation. READ COMMITTED is required so checks made after waiting observe a
newly committed fence. The lock survives successful operation savepoints until the
outer transaction commits. It serializes control-plane credential writes; inference
never acquires it. Memory uses its existing transaction mutex and rollback snapshot.
File mode refuses fence writes.

Every writer must implement fences before installing any. The table migration is
additive, but older binaries do not enforce the records: a mixed-version rollout
or rollback to an older writer cannot provide the guarantee. The deployment must
exclude those writers before enabling fence use. Fences remain permanently stored;
returning access requires a different Key name.
