package proxy

import (
	"github.com/mostlygeek/llama-swap/proxy/config"
)

// applyConfigAndSyncProcessGroups atomically swaps in a new config and keeps
// runtime process groups in sync so newly managed models are immediately
// loadable/unloadable from the UI.
func (pm *ProxyManager) applyConfigAndSyncProcessGroups(newConfig config.Config) {
	pm.Lock()
	oldGroups := pm.processGroups

	nextGroups := make(map[string]*ProcessGroup, len(newConfig.Groups))
	groupsToShutdown := make([]*ProcessGroup, 0)
	processesToShutdown := make([]*Process, 0)

	for groupID, nextGroupCfg := range newConfig.Groups {
		if oldGroup, ok := oldGroups[groupID]; ok {
			removedProcesses := syncExistingGroupRuntime(
				oldGroup,
				nextGroupCfg,
				newConfig,
				pm.proxyLogger,
				pm.upstreamLogger,
			)
			processesToShutdown = append(processesToShutdown, removedProcesses...)
			nextGroups[groupID] = oldGroup
			continue
		}
		nextGroups[groupID] = NewProcessGroup(groupID, newConfig, pm.proxyLogger, pm.upstreamLogger)
	}

	for groupID, oldGroup := range oldGroups {
		if _, ok := nextGroups[groupID]; !ok {
			groupsToShutdown = append(groupsToShutdown, oldGroup)
		}
	}

	pm.config = newConfig
	pm.processGroups = nextGroups
	pm.Unlock()

	for _, process := range processesToShutdown {
		process.Shutdown()
	}

	for _, group := range groupsToShutdown {
		group.Shutdown()
	}
}

func syncExistingGroupRuntime(
	group *ProcessGroup,
	nextGroupCfg config.GroupConfig,
	newConfig config.Config,
	proxyLogger *LogMonitor,
	upstreamLogger *LogMonitor,
) []*Process {
	group.Lock()
	defer group.Unlock()

	group.config = newConfig
	group.swap = nextGroupCfg.Swap
	group.exclusive = nextGroupCfg.Exclusive
	group.persistent = nextGroupCfg.Persistent

	nextProcesses := make(map[string]*Process, len(nextGroupCfg.Members))
	nextMembers := make(map[string]struct{}, len(nextGroupCfg.Members))

	for _, member := range nextGroupCfg.Members {
		modelCfg, resolvedName, found := newConfig.FindConfig(member)
		if !found {
			continue
		}

		nextMembers[resolvedName] = struct{}{}
		existing := group.processes[resolvedName]
		if existing == nil {
			if fallback := group.processes[member]; fallback != nil {
				existing = fallback
			}
		}

		// Preserve active process objects to avoid losing runtime state when
		// config reload only reorders members or reassigns ${PORT}.
		if existing != nil && existing.CurrentState() != StateStopped {
			nextProcesses[resolvedName] = existing
			continue
		}

		processLogger := NewLogMonitorWriter(upstreamLogger)
		nextProcesses[resolvedName] = NewProcess(
			resolvedName,
			newConfig.HealthCheckTimeout,
			modelCfg,
			processLogger,
			proxyLogger,
		)
	}

	removedProcesses := make([]*Process, 0)
	for existingID, process := range group.processes {
		if _, keep := nextMembers[existingID]; keep {
			continue
		}
		switch process.CurrentState() {
		case StateStopped, StateShutdown:
			// Nothing to do.
		default:
			removedProcesses = append(removedProcesses, process)
		}
	}

	group.processes = nextProcesses
	if _, ok := group.processes[group.lastUsedProcess]; !ok {
		group.lastUsedProcess = ""
	}

	return removedProcesses
}
