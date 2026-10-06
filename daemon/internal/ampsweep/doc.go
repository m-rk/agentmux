// Package ampsweep archives junk amp threads that belong to no task
// instance, so nobody has to do it by hand. A daily job (`agentmux amp
// sweep`, run from the gc timer) lists this host's amp account threads
// and archives the ones that are clearly junk:
//
//   - the first message starts "[relayed by" or "[sent by" (a relay
//     stray: a sessions send that never reached a live worker);
//   - 6 messages or fewer, older than 6 hours, and neither a dispatched
//     worker thread nor a nightly review (probes like "ok" or "reply
//     with exactly ...");
//   - an untitled thread in error state older than an hour;
//   - a nightly review thread older than 3 days.
//
// Archiving is reversible (`amp threads archive --unarchive`), and the
// existing gc retention (14 days) then deletes the archived junk. Sweep
// never deletes anything itself, and never touches a thread with more
// than 6 messages, a dispatched worker of a live task, or anything
// younger than the limits above.
package ampsweep
