import Machines.Event

/-!
Claims about the journal row table, proved over the table that
`internal/lifecycle.EventTable` renders into `Machines/Event.lean`.
-/

namespace Proofs.Event
open Machines.Event Statemachine

/-- A removed row stays removed: nothing follows `removed`. -/
theorem removed_stays (s : State) (r : Reaches machine .removed s) : s = .removed :=
  machine.terminal_stays terminal_removed r

/-- An acknowledged row is never pending again, so a delivered event is not
delivered a second time by the row's own state. -/
theorem acknowledged_never_pending (s : State) (r : Reaches machine .acknowledged s) :
    s ≠ .pending := by
  have := machine.reaches_closed (I := [State.acknowledged, .removed]) (by decide) r (by decide)
  intro h; subst h; simp at this

/-- Only a pending row is acknowledged: every predecessor of `acknowledged`
is `pending`. -/
theorem acknowledged_only_from_pending :
    ∀ s ∈ machine.states, State.acknowledged ∈ machine.next s → s = .pending := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Event
