import Machines.Tunnel

/-!
Claims about the tunnel status table, proved over the table that
`internal/lifecycle.TunnelTable` renders into `Machines/Tunnel.lean`.
-/

namespace Proofs.Tunnel
open Machines.Tunnel Statemachine

/-- A Provider that is connected can disconnect and one that is disconnected
can connect: neither state is final. -/
theorem neither_state_is_final :
    ¬ machine.Terminal .connected ∧ ¬ machine.Terminal .disconnected := by
  decide

/-- Concurrent writers that check the state they land on keep the table,
whatever they read and in whatever order their writes land. -/
theorem guarded_writers_follow (ws : List (Write State)) (row : State) :
    Reaches machine row (ws.foldl machine.guarded row) :=
  machine.guarded_follows ws row

end Proofs.Tunnel
