package routes

import (
	"net/http"
	"testing"
)

func TestOrgDataViewCRUDRoutesRegistered(t *testing.T) {
	paths := registeredPaths(t)
	expected := []string{
		http.MethodGet + " /orgs/:orgId/data-objects/:objectId/views",
		http.MethodPost + " /orgs/:orgId/data-objects/:objectId/views",
		http.MethodPut + " /orgs/:orgId/data-objects/:objectId/views/:viewId",
		http.MethodDelete + " /orgs/:orgId/data-objects/:objectId/views/:viewId",
	}
	for _, route := range expected {
		if !paths[route] {
			t.Errorf("ruta de vistas no registrada: %s", route)
		}
	}
}
