package hubcore

import "testing"

func TestExplicitResumePreservesNewerAliasRecovery(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "during exit", true: "after exit"}[complete], func(t *testing.T) {
			locks := NewResumeLocks()
			finishOld := locks.BeginForceStop([]string{"A", "B"})
			finishOld(true)
			epochA := locks.RecoveryState("A").Epoch
			finishNew := locks.BeginForceStop([]string{"B", "C"})
			if complete {
				finishNew(true)
			}
			beforeB := locks.RecoveryState("B")
			beforeC := locks.RecoveryState("C")
			if err := locks.ExplicitResumeCompleted("A", epochA); err != nil {
				t.Fatal(err)
			}
			if locks.RecoveryState("A").ResumeRequired {
				t.Fatal("explicitly resumed alias remains fenced")
			}
			if after := locks.RecoveryState("B"); after.ResumeRequired != beforeB.ResumeRequired || after.Stopping != beforeB.Stopping {
				t.Fatalf("older resume changed newer B recovery: before=%+v after=%+v", beforeB, after)
			}
			if after := locks.RecoveryState("C"); after.ResumeRequired != beforeC.ResumeRequired || after.Stopping != beforeC.Stopping {
				t.Fatalf("older resume changed newer C recovery: before=%+v after=%+v", beforeC, after)
			}
			if !complete {
				finishNew(true)
			}
		})
	}
}
