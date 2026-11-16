import Machines.Health

/-!
Claims about the Provider health table, proved over the table that
`internal/lifecycle.HealthTable` renders into `Machines/Health.lean`.
-/

namespace Proofs.Health
open Machines.Health Statemachine

/-- A failure never softens an unreachable Provider: `Unreachable` has no
step to `Degraded`, only to `Healthy` by a success or `Unknown` by a reset. -/
theorem unreachable_never_degraded_directly :
    State.«Degraded» ∉ machine.next .«Unreachable» := by
  decide

/-- Degraded is entered only from unknown or healthy, by a failure. -/
theorem degraded_only_from_unknown_or_healthy :
    ∀ s ∈ machine.states, State.«Degraded» ∈ machine.next s →
      s = .«Unknown» ∨ s = .«Healthy» := by
  decide

/-- A success reaches healthy from every other state. -/
theorem healthy_reached_from_every_state :
    ∀ s ∈ machine.states, s ≠ .«Healthy» → State.«Healthy» ∈ machine.next s := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Health
