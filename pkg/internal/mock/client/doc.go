//go:generate mockgen -package=client -destination=mocks.go github.com/gardener/gardener-extension-provider-gcp/pkg/internal/client Interface,FirewallsService,RoutesService,InstancesService,DisksService,RegionsService,FirewallsListCall,FirewallsGetCall,FirewallsInsertCall,FirewallsPatchCall,FirewallsDeleteCall,RoutesDeleteCall,RoutesListCall,InstancesGetCall,InstancesDeleteCall,InstancesInsertCall,DisksInsertCall,DisksGetCall,DisksDeleteCall,RegionsGetCall

package client