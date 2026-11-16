------------------------------- MODULE Lease --------------------------------
(***************************************************************************)
(* The lease that keeps a background job (health, discovery, journal       *)
(* delivery, usage) on one replica at a time (store.Leases). Acquire       *)
(* takes the row when it is free, lapsed or already the caller's, and      *)
(* sets its expiry Lease seconds ahead; a holder renews every Renew        *)
(* seconds and acts on the job from one acquire until the next, for as     *)
(* long as its last acquire answered true. A renewal may run late: the     *)
(* process stalls, or the call waits on the database. Time is in whole     *)
(* seconds and every replica reads the same clock. Lease and Renew are    *)
(* store.LeaseTTL and store.LeaseRenew, rendered from the Go constants     *)
(* into LeaseConstants.tla by TestLeaseConstants.                          *)
(***************************************************************************)
EXTENDS Integers, LeaseConstants

CONSTANTS Replicas, Horizon, MaxLate

None == "none"

VARIABLES
    clock,    \* seconds since the start
    holder,   \* the replica the row names, or None
    expires,  \* the clock at which the row lapses
    acting,   \* replica -> TRUE while its last acquire answered true
    due       \* replica -> the clock its next renewal is due at

vars == <<clock, holder, expires, acting, due>>

Init == /\ clock = 0
        /\ holder = None
        /\ expires = 0
        /\ acting = [r \in Replicas |-> FALSE]
        /\ due = [r \in Replicas |-> 0]

\* Acquire: one statement that takes the row when it is the caller's or has
\* lapsed, and answers whether it did. The replica acts from then until its
\* next acquire, whatever the row says in between.
Acquire(r) ==
    /\ clock >= due[r]
    /\ \/ /\ (holder = r \/ holder = None \/ expires <= clock)
          /\ holder' = r
          /\ expires' = clock + Lease
          /\ acting' = [acting EXCEPT ![r] = TRUE]
       \/ /\ ~(holder = r \/ holder = None \/ expires <= clock)
          /\ acting' = [acting EXCEPT ![r] = FALSE]
          /\ UNCHANGED <<holder, expires>>
    /\ due' = [due EXCEPT ![r] = clock + Renew]
    /\ UNCHANGED clock

\* A second passes, unless a renewal is already MaxLate seconds late.
Tick ==
    /\ clock < Horizon
    /\ \A r \in Replicas : clock < due[r] + MaxLate
    /\ clock' = clock + 1
    /\ UNCHANGED <<holder, expires, acting, due>>

\* The model stops at Horizon seconds.
Stop == clock = Horizon /\ UNCHANGED vars

Next == Tick \/ Stop \/ \E r \in Replicas : Acquire(r)

Spec == Init /\ [][Next]_vars

\* At most one replica acts on the job at a time.
Exclusive == \A r, s \in Replicas : (acting[r] /\ acting[s]) => r = s

TypeOK == /\ clock \in 0..Horizon
          /\ holder \in Replicas \cup {None}

\* The longest a renewal may run late with the exclusion intact: it must
\* end before the row lapses, and a replica that finds the row lapsed in
\* the same second takes it first. Lease.cfg uses it.
SafeLate == Lease - Renew - 1

\* A renewal as late as the whole lease: the row lapses and a second
\* replica takes it while the first still acts. Lease.late.cfg uses it.
LateLate == Lease
=============================================================================
