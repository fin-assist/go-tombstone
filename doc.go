// Package tombstone prevents deleted users from recreating service data.
//
// Services apply Migrations, construct Store with their transaction provider and
// primary reader, and install Interceptors after identity verification and caller
// authorization. NewDeletionServer implements the generated PurgeUserData RPC.
//
// Every write must call Store.Guard in the same READ COMMITTED transaction as its
// effect. Store.Purge shares its per-user transaction lock, so deletion either
// removes an earlier write or prevents a later one. The service owns transaction
// lifetime and must roll back on errors. Tombstones are retained indefinitely.
package tombstone
