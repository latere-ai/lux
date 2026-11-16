import Machines.Object

/-!
Claims about the object row table, proved over the table that
`internal/lifecycle.ObjectTable` renders into `Machines/Object.lean`.
-/

namespace Proofs.Object
open Machines.Object Statemachine

/-- A pruned row stays pruned: nothing follows `pruned`. -/
theorem pruned_stays (s : State) (r : Reaches machine .pruned s) : s = .pruned :=
  machine.terminal_stays terminal_pruned r

/-- A deleted row is never live again, so a name freed by a delete and an id
that is never freed cannot come back. -/
theorem deleted_never_live (s : State) (r : Reaches machine .deleted s) :
    s ≠ .live := by
  have := machine.reaches_closed (I := [State.deleted, .pruned]) (by decide) r (by decide)
  intro h; subst h; simp at this

/-- Only a deleted row is pruned: every predecessor of `pruned` is
`deleted`. -/
theorem pruned_only_after_delete :
    ∀ s ∈ machine.states, State.pruned ∈ machine.next s → s = .deleted := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Object
