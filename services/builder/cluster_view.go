package main

import (
	"github.com/berops/claudie/proto/pb"
	"google.golang.org/protobuf/proto"
)

// ClusterView contains the per-cluster view on a given config.
// No mutex is needed when processing concurrently as long as each cluster only
// works with related values.
type ClusterView struct {
	// CurrentClusters are the individual clusters defined in the kubernetes section of the config of the current state.
	CurrentClusters map[string]*pb.K8Scluster
	// DesiredClusters are the individual clusters defined in the kubernetes section of the config of the desired state.
	DesiredClusters map[string]*pb.K8Scluster

	// Loadbalancers are the loadbalancers attach to a given kubernetes cluster in the current state.
	Loadbalancers map[string][]*pb.LBcluster
	// DesiredLoadbalancers are the loadbalancers attach to a given kubernetes cluster in the desired state.
	DesiredLoadbalancers map[string][]*pb.LBcluster

	// DeletedLoadbalancers are the loadbalancers that will be deleted (present in the current state but missing in the desired state)
	DeletedLoadbalancers map[string][]*pb.LBcluster

	// ClusterWorkflows is additional information per-cluster workflow (current stage of execution, if any error occurred etc..)
	ClusterWorkflows map[string]*pb.Workflow
}

func NewClusterView(config *pb.Config) *ClusterView {
	var (
		clusterWorkflows     = make(map[string]*pb.Workflow)
		clusters             = make(map[string]*pb.K8Scluster)
		desiredClusters      = make(map[string]*pb.K8Scluster)
		loadbalancers        = make(map[string][]*pb.LBcluster)
		desiredLoadbalancers = make(map[string][]*pb.LBcluster)
		deletedLoadbalancers = make(map[string][]*pb.LBcluster)
	)

	for _, current := range config.CurrentState.Clusters {
		clusters[current.ClusterInfo.Name] = current

		// store the cluster name with default workflow state.
		clusterWorkflows[current.ClusterInfo.Name] = &pb.Workflow{
			Stage:  pb.Workflow_NONE,
			Status: pb.Workflow_IN_PROGRESS,
		}
	}

	for _, desired := range config.DesiredState.Clusters {
		desiredClusters[desired.ClusterInfo.Name] = desired

		// store the cluster name with default workflow state.
		clusterWorkflows[desired.ClusterInfo.Name] = &pb.Workflow{
			Stage:  pb.Workflow_NONE,
			Status: pb.Workflow_IN_PROGRESS,
		}
	}

	for _, current := range config.CurrentState.LoadBalancerClusters {
		loadbalancers[current.TargetedK8S] = append(loadbalancers[current.TargetedK8S], current)
	}

	for _, desired := range config.DesiredState.LoadBalancerClusters {
		desiredLoadbalancers[desired.TargetedK8S] = append(desiredLoadbalancers[desired.TargetedK8S], desired)
	}

Lb:
	for _, current := range config.CurrentState.LoadBalancerClusters {
		for _, desired := range config.DesiredState.LoadBalancerClusters {
			if desired.ClusterInfo.Name == current.ClusterInfo.Name && desired.ClusterInfo.Hash == current.ClusterInfo.Hash {
				continue Lb
			}
		}
		deletedLoadbalancers[current.TargetedK8S] = append(deletedLoadbalancers[current.TargetedK8S], proto.Clone(current).(*pb.LBcluster))
	}

	return &ClusterView{
		CurrentClusters:      clusters,
		DesiredClusters:      desiredClusters,
		Loadbalancers:        loadbalancers,
		DesiredLoadbalancers: desiredLoadbalancers,
		DeletedLoadbalancers: deletedLoadbalancers,
		ClusterWorkflows:     clusterWorkflows,
	}
}

// MergeChanges propagates the changes made back to the config.
func (view *ClusterView) MergeChanges(config *pb.Config) {
	config.State = view.ClusterWorkflows

	for i, current := range config.CurrentState.Clusters {
		if updated, ok := view.CurrentClusters[current.ClusterInfo.Name]; ok {
			config.CurrentState.Clusters[i] = updated
		}
	}

	for i, desired := range config.DesiredState.Clusters {
		if updated, ok := view.DesiredClusters[desired.ClusterInfo.Name]; ok {
			config.DesiredState.Clusters[i] = updated
		}
	}

	for i, current := range config.CurrentState.LoadBalancerClusters {
		for _, lb := range view.Loadbalancers[current.TargetedK8S] {
			if current.ClusterInfo.Name == lb.ClusterInfo.Name {
				config.CurrentState.LoadBalancerClusters[i] = lb
				break
			}
		}
	}

	for i, desired := range config.DesiredState.LoadBalancerClusters {
		for _, lb := range view.DesiredLoadbalancers[desired.TargetedK8S] {
			if desired.ClusterInfo.Name == lb.ClusterInfo.Name {
				config.DesiredState.LoadBalancerClusters[i] = lb
				break
			}
		}
	}
}

// UpdateFromBuild takes the changes after a successful workflow of a given cluster
func (view *ClusterView) UpdateFromBuild(ctx *BuilderContext) {
	if ctx.cluster != nil {
		view.CurrentClusters[ctx.cluster.ClusterInfo.Name] = ctx.cluster
	}

	if ctx.desiredCluster != nil {
		view.DesiredClusters[ctx.desiredCluster.ClusterInfo.Name] = ctx.desiredCluster
	}

	if ctx.Workflow != nil {
		view.ClusterWorkflows[ctx.GetClusterName()] = ctx.Workflow
	}

	for _, current := range ctx.loadbalancers {
		for i := range view.Loadbalancers[current.TargetedK8S] {
			if view.Loadbalancers[current.TargetedK8S][i].ClusterInfo.Name == current.ClusterInfo.Name {
				view.Loadbalancers[current.TargetedK8S][i] = current
				break
			}
		}
	}

	for _, desired := range ctx.desiredLoadbalancers {
		for i := range view.DesiredLoadbalancers[desired.TargetedK8S] {
			if view.DesiredLoadbalancers[desired.TargetedK8S][i].ClusterInfo.Name == desired.ClusterInfo.Name {
				view.DesiredLoadbalancers[desired.TargetedK8S][i] = desired
				break
			}
		}
	}

	for _, deleted := range ctx.deletedLoadBalancers {
		for i := range view.DeletedLoadbalancers[deleted.TargetedK8S] {
			if view.DeletedLoadbalancers[deleted.TargetedK8S][i].ClusterInfo.Name == deleted.ClusterInfo.Name {
				view.DeletedLoadbalancers[deleted.TargetedK8S][i] = deleted
				break
			}
		}
	}
}

// AllClusters returns a slice of cluster all cluster names, from both the current state and desired state.
// This is useful to be abe to distinguish which clusters were deleted and which were not.
func (view *ClusterView) AllClusters() []string {
	clusters := make(map[string]struct{})

	for _, current := range view.CurrentClusters {
		clusters[current.ClusterInfo.Name] = struct{}{}
	}

	for _, desired := range view.DesiredClusters {
		clusters[desired.ClusterInfo.Name] = struct{}{}
	}

	c := make([]string, 0, len(clusters))
	for k := range clusters {
		c = append(c, k)
	}

	return c
}

func (view *ClusterView) SetWorkflowError(clusterName string, err error) {
	view.ClusterWorkflows[clusterName].Status = pb.Workflow_ERROR
	view.ClusterWorkflows[clusterName].Description = err.Error()
}

func (view *ClusterView) SetWorkflowDone(clusterName string) {
	view.ClusterWorkflows[clusterName].Status = pb.Workflow_DONE
	view.ClusterWorkflows[clusterName].Stage = pb.Workflow_NONE
	view.ClusterWorkflows[clusterName].Description = ""
}
