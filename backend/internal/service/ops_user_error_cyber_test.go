package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMapUserErrorCategoryCyber(t *testing.T) {
	require.Equal(t, "cyber", MapUserErrorCategory("request", "cyber_policy"))
	phases, types := CategoryToFilter("cyber")
	require.Equal(t, []string{"request"}, phases)
	require.Equal(t, []string{"cyber_policy"}, types)
}

func TestOwnErrorListNeverFallsBackToDiagnosticMessage(t *testing.T) {
	e := OpsErrorLog{Message: "private upstream diagnostic"}
	require.Empty(t, ToUserErrorRequest(&e).Message)
	require.Empty(t, toObserverErrorRequest(&e).Message)
	e.ClientMessage = "message after rule"
	require.Equal(t, e.ClientMessage, ToUserErrorRequest(&e).Message)
	require.Equal(t, e.ClientMessage, toObserverErrorRequest(&e).Message)
	require.Equal(t, "private upstream diagnostic", e.Message)
}
