import Machines.Session

/-!
Claims about the tunnel session table, proved over the table that
`internal/lifecycle.SessionTable` renders into `Machines/Session.lean`.
-/

namespace Proofs.Session
open Machines.Session Statemachine

/-- A closed session stays closed: nothing follows `closed`, so a second
close, from any of the paths that end a session, ends nothing. -/
theorem closed_stays (s : State) (r : Reaches machine .closed s) : s = .closed :=
  machine.terminal_stays terminal_closed r

/-- A closed session is never live again. -/
theorem closed_never_live (s : State) (r : Reaches machine .closed s) :
    s ≠ .live := by
  have := machine.terminal_stays terminal_closed r
  subst this; decide

end Proofs.Session
