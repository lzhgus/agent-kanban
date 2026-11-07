package main

// Auto pickup: with Auto on, the To Do column works itself off. Tasks go,
// first filed first, to an agent that is free: the one named, or for a
// task left to "any" agent, the free one with the most of its plan left.
// An agent picks one task at a time. A task an agent filed plans first, so
// a chain of agents filing work for each other still stops for you before
// anything changes.

import (
	"os/exec"
	"slices"
)

// anyAgent leaves a task to whichever agent is free when it starts.
const anyAgent = "any"

var agentChoices = append([]string{anyAgent}, agentNames...)

var agentBins = map[string]string{"claude": "claude", "antigravity": "agy", "copilot": "copilot", "grok": "grok", "pi": "pi",
	"opencode": "opencode", "kimi": "kimi"}

// installed reports whether an agent's CLI is on PATH.
// ponytail: a PATH lookup each call; cache it if pickups ever get frequent.
var installed = func(agent string) bool {
	_, err := exec.LookPath(agentBins[agent])
	return err == nil
}

// busy reports whether an agent works on a task it picked up.
func (b *board) busy(agent string) bool {
	for _, t := range b.Tasks {
		if t.picked && t.Agent == agent && started(t) {
			return true
		}
	}
	return false
}

// pickAgent chooses who takes a task left to any agent: an installed agent,
// signed in as far as its quota says, with no window spent, free if free
// says so, the one with the most of its plan left; "" if none can.
func (b *board) pickAgent(free bool) string {
	best, bestUsed := "", 101.0
	for _, a := range agentNames {
		if !installed(a) || (free && b.busy(a)) {
			continue
		}
		used := 0.0
		if q := b.quotas[a]; q != nil {
			if q.Err != "" {
				continue
			}
			for _, w := range q.Windows {
				used = max(used, w.Used)
			}
		}
		if used < 100 && used < bestUsed {
			best, bestUsed = a, used
		}
	}
	return best
}

// autopick starts what Auto may start: To Do tasks, oldest first, while
// slots are free, each to a free agent.
func (b *board) autopick() {
	if !b.AutoPickup {
		return
	}
	for _, t := range slices.Clone(b.Tasks) {
		if len(b.runs) >= b.maxRunning() {
			return // auto pickup takes free slots only: it never queues
		}
		if t.Status != Todo {
			continue
		}
		agent := t.Agent
		if agent == anyAgent {
			agent = b.pickAgent(true)
		} else if b.busy(agent) || !installed(agent) {
			agent = ""
		}
		if agent == "" {
			continue
		}
		t.Agent, t.picked = agent, true
		if t.ByAgent && !t.PlanFirst {
			t.PlanFirst = true // an agent's task waits for you before it changes anything
			t.addLog("note: filed by an agent: it plans first, for you to confirm")
		}
		t.addLog("── picked up by " + agent + " (auto)")
		b.start(t)
	}
}

func (b *board) setAutoPickup(on bool) {
	b.AutoPickup = on
	b.save()
	b.autopick()
}
