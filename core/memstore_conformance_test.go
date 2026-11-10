package core_test

import (
	"testing"

	"github.com/streetsmartops/incident-management-chatbot/core"
	"github.com/streetsmartops/incident-management-chatbot/test/storetest"
)

func TestMemStoreConformance(t *testing.T) {
	storetest.Run(t, func(*testing.T) core.Store { return core.NewMemStore() })
}

func TestMemStoreMultiReplica(t *testing.T) { storetest.RunMultiReplica(t, core.NewMemStore()) }
