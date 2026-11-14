// Package proc is what emx needs from the operating system about processes: whether one is still the
// same process that was started, which processes belong to an engine, how much memory they hold, and how to
// stop them. Each platform file implements:
//
//	Start(pid)            a token for this incarnation of pid (0 when gone or a zombie); equal tokens mean
//	                      the same process, so a reused pid is told apart
//	Members(leader, started)
//	                      live processes of the engine started as leader (its session, group or tree);
//	                      started is the leader's Start token when known (0 otherwise), which keeps
//	                      processes of a later owner of the same process id out
//	RSSMiB(pids)          their resident memory
//	Detach(cmd)           start cmd as the leader of its own group, so it outlives the caller and is
//	                      stopped as a whole
//	Signal(leader, sig)   ask the engine to stop; nil when it is already gone
//	Kill(leader, started) force the engine and every member to exit
package proc
