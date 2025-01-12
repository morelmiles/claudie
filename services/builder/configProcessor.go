package main

/*
How operations with the nodes work:

We can have three cases of a operation within the input manifest

- just addition of a nodes
  - the config is processed right away

- just deletion of a nodes
  - firstly, the nodes are deleted from the cluster (via kubectl)
  - secondly, the config is  processed which will delete the nodes from infra

- addition AND deletion of the nodes
  - firstly the tmpConfig is applied, which will only add nodes into the cluster
  - secondly, the nodes are deleted from the cluster (via kubectl)
  - lastly, the config is processed, which will delete the nodes from infra
*/

import (
	"fmt"
	"sync"

	"github.com/berops/claudie/internal/utils"
	"github.com/berops/claudie/proto/pb"
	cbox "github.com/berops/claudie/services/context-box/client"
	"github.com/rs/zerolog/log"
	"google.golang.org/protobuf/proto"
)

// configProcessor will fetch new configs from the context-box service. Each received config will be processed in
// a separate go-routine. If a sync.WaitGroup is supplied it will call the Add(1) and then the Done() method on it
// after the go-routine finishes the work, if nil it will be ignored.
func configProcessor(c pb.ContextBoxServiceClient, wg *sync.WaitGroup) error {
	res, err := cbox.GetConfigBuilder(c) // Get a new config
	if err != nil {
		return fmt.Errorf("error while getting config from the Context-box: %w", err)
	}

	config := res.GetConfig()
	if config == nil {
		return nil
	}

	if wg != nil {
		// we received a non-nil config thus we add a new worker to the wait group.
		wg.Add(1)
	}

	go func() {
		if wg != nil {
			defer wg.Done()
		}

		clusterView := NewClusterView(config)

		// if Desired state is null and current is not we delete the infra for the config.
		if config.DsChecksum == nil && config.CsChecksum != nil {
			if err := destroyConfig(config, clusterView, c); err != nil {
				// Save error to DB.
				log.Error().Msgf("Error while destroying config %s : %v", config.Name, err)
				if err := saveConfigWithWorkflowError(config, c, clusterView); err != nil {
					log.Error().Msgf("Failed to save error message for config %s:  %v", config.Name, err)
				}
			}
			return
		}

		if err := utils.ConcurrentExec(clusterView.AllClusters(), func(clusterName string) error {
			// Check if we need to destroy the cluster or any Loadbalancers
			done, err := destroy(config.Name, clusterName, clusterView, c)
			if err != nil {
				clusterView.SetWorkflowError(clusterName, err)
				log.Error().Msgf("Error while destroying cluster %s project %s : %v", clusterName, config.Name, err)
				return err
			}

			if done {
				clusterView.SetWorkflowDone(clusterName)
				log.Info().Msgf("Finished workflow for cluster %s project %s", clusterName, config.Name)
				return updateWorkflowStateInDB(config.Name, clusterName, clusterView.ClusterWorkflows[clusterName], c)
			}

			// Handle deletion and addition of nodes.
			tmpDesired, toDelete := stateDifference(clusterView.CurrentClusters[clusterName], clusterView.DesiredClusters[clusterName])
			if tmpDesired != nil {
				clusterView.ClusterWorkflows[clusterName].Description = "Processing stage [1/2]"
				log.Info().Msgf("Processing stage [1/2] for cluster %s config %s", clusterName, config.Name)

				ctx := &BuilderContext{
					projectName:          config.Name,
					cluster:              clusterView.CurrentClusters[clusterName],
					desiredCluster:       tmpDesired,
					loadbalancers:        clusterView.Loadbalancers[clusterName],
					desiredLoadbalancers: clusterView.DesiredLoadbalancers[clusterName],
					deletedLoadBalancers: clusterView.DeletedLoadbalancers[clusterName],
					Workflow:             clusterView.ClusterWorkflows[clusterName],
				}

				if ctx, err = buildCluster(ctx, c); err != nil {
					clusterView.SetWorkflowError(clusterName, err)
					log.Error().Msgf("Failed to build cluster %s project %s : %v", clusterName, config.Name, err)
					return err
				}
				log.Info().Msgf("First stage for cluster %s project %s finished building", clusterName, config.Name)

				// make the desired state of the temporary cluster the new current state.
				clusterView.CurrentClusters[clusterName] = ctx.desiredCluster
				clusterView.Loadbalancers[clusterName] = ctx.desiredLoadbalancers
			}

			if toDelete != nil {
				clusterView.ClusterWorkflows[clusterName].Stage = pb.Workflow_DELETE_NODES
				if err := updateWorkflowStateInDB(config.Name, clusterName, clusterView.ClusterWorkflows[clusterName], c); err != nil {
					clusterView.SetWorkflowError(clusterName, err)
					return err
				}
				log.Info().Msgf("Deleting nodes from cluster %s project %s", clusterName, config.Name)
				if clusterView.CurrentClusters[clusterName], err = deleteNodes(clusterView.CurrentClusters[clusterName], toDelete); err != nil {
					clusterView.SetWorkflowError(clusterName, err)
					log.Error().Msgf("Failed to delete nodes cluster %s project %s : %v", clusterName, config.Name, err)
					return err
				}
			}

			message := fmt.Sprintf("Processing cluster %s config %s", clusterName, config.Name)
			if tmpDesired != nil {
				clusterView.ClusterWorkflows[clusterName].Description = "Processing stage [2/2]"
				message = fmt.Sprintf("Processing stage [2/2] for cluster %s config %s", clusterName, config.Name)
			}
			log.Info().Msgf(message)

			ctx := &BuilderContext{
				projectName:          config.Name,
				cluster:              clusterView.CurrentClusters[clusterName],
				desiredCluster:       clusterView.DesiredClusters[clusterName],
				loadbalancers:        clusterView.Loadbalancers[clusterName],
				desiredLoadbalancers: clusterView.DesiredLoadbalancers[clusterName],
				deletedLoadBalancers: clusterView.DeletedLoadbalancers[clusterName],
				Workflow:             clusterView.ClusterWorkflows[clusterName],
			}

			if ctx, err = buildCluster(ctx, c); err != nil {
				clusterView.SetWorkflowError(clusterName, err)
				log.Error().Msgf("Failed to build cluster %s project %s : %v", clusterName, config.Name, err)
				return err
			}

			clusterView.SetWorkflowDone(clusterName)

			if err := updateWorkflowStateInDB(config.Name, clusterName, ctx.Workflow, c); err != nil {
				clusterView.SetWorkflowError(clusterName, err)
				log.Error().Msgf("failed to save workflow for cluster %s project %s: %s", clusterName, config.Name, err)
				return err
			}

			// Propagate the changes made to the cluster back to the View.
			clusterView.UpdateFromBuild(ctx)
			log.Info().Msgf("Finished building cluster %s project %s", clusterName, config.Name)
			return nil
		}); err != nil {
			log.Error().Msgf("Error encountered while processing config %s : %v", config.Name, err)
			if err := saveConfigWithWorkflowError(config, c, clusterView); err != nil {
				log.Error().Msgf("Failed to save error message due to: %s", err)
			}
			return
		}

		// Propagate all the changes to the config.
		clusterView.MergeChanges(config)

		// Update the config and store it to the DB.
		log.Debug().Msgf("Saving the config %s", config.Name)
		config.CurrentState = config.DesiredState
		if err := cbox.SaveConfigBuilder(c, &pb.SaveConfigRequest{Config: config}); err != nil {
			log.Error().Msgf("error while saving the config %s: %s", config.GetName(), err)
			return
		}

		log.Info().Msgf("Config %s finished building", config.Name)
	}()

	return nil
}

// stateDifference takes config to calculates difference between desired and current state to determine how many nodes  needs to be deleted and added.
func stateDifference(current *pb.K8Scluster, desired *pb.K8Scluster) (*pb.K8Scluster, map[string]int32) {
	desired = proto.Clone(desired).(*pb.K8Scluster)

	currentNodepoolCounts := nodepoolsCounts(current)
	delCounts, adding, deleting := findNodepoolDifference(currentNodepoolCounts, desired)

	//if any key left, it means that nodepool is defined in current state but not in the desired, i.e. whole nodepool should be deleted
	if len(currentNodepoolCounts) > 0 {
		deleting = true
		// let delCounts hold all delete counts
		mergeDeleteCounts(delCounts, currentNodepoolCounts)

		// add the deleted nodes to the Desired state
		if current != nil && desired != nil {
			//append nodepool to desired state, since tmpConfig only adds nodes
			for nodepoolName := range currentNodepoolCounts {
				log.Debug().Msgf("Nodepool %s from cluster %s will be deleted", nodepoolName, current.ClusterInfo.Name)
				desired.ClusterInfo.NodePools = append(desired.ClusterInfo.NodePools, utils.GetNodePoolByName(nodepoolName, current.ClusterInfo.GetNodePools()))
			}
		}
	}

	switch {
	case adding && deleting:
		return desired, delCounts
	case deleting:
		return nil, delCounts
	default:
		return nil, nil
	}
}

// nodepoolsCounts returns a map for the counts in each nodepool for a cluster.
func nodepoolsCounts(cluster *pb.K8Scluster) map[string]int32 {
	counts := make(map[string]int32)

	for _, nodePool := range cluster.GetClusterInfo().GetNodePools() {
		counts[nodePool.Name] = nodePool.Count
	}

	return counts
}

func findNodepoolDifference(currentNodepoolCounts map[string]int32, desiredClusterTmp *pb.K8Scluster) (result map[string]int32, adding, deleting bool) {
	nodepoolCountToDelete := make(map[string]int32)

	for _, nodePoolDesired := range desiredClusterTmp.GetClusterInfo().GetNodePools() {
		currentCount, ok := currentNodepoolCounts[nodePoolDesired.Name]
		if !ok {
			// not in current state, adding.
			adding = true
			continue
		}

		if nodePoolDesired.Count > currentCount {
			adding = true
		}

		var countToDelete int32

		if nodePoolDesired.Count < currentCount {
			deleting = true
			countToDelete = currentCount - nodePoolDesired.Count

			// since we are working with tmp config, we do not delete nodes in this step, thus save the current node count
			nodePoolDesired.Count = currentCount
		}

		nodepoolCountToDelete[nodePoolDesired.Name] = countToDelete

		// keep track of which nodepools were deleted
		delete(currentNodepoolCounts, nodePoolDesired.Name)
	}

	return nodepoolCountToDelete, adding, deleting
}

func mergeDeleteCounts(dst, src map[string]int32) map[string]int32 {
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// separateNodepools creates two slices of node names, one for master and one for worker nodes
func separateNodepools(clusterNodes map[string]int32, clusterInfo *pb.ClusterInfo) (master []string, worker []string) {
	for _, nodepool := range clusterInfo.NodePools {
		if count, ok := clusterNodes[nodepool.Name]; ok && count > 0 {
			names := getNodeNames(nodepool, int(count))
			if nodepool.IsControl {
				master = append(master, names...)
			} else {
				worker = append(worker, names...)
			}
		}
	}
	return master, worker
}

// getNodeNames returns slice of length count with names of the nodes from specified nodepool
// nodes chosen are from the last element in Nodes slice, up to the first one
func getNodeNames(nodepool *pb.NodePool, count int) (names []string) {
	for i := len(nodepool.Nodes) - 1; i >= len(nodepool.Nodes)-count; i-- {
		names = append(names, nodepool.Nodes[i].Name)
		log.Debug().Msgf("Choosing node %s for deletion", nodepool.Nodes[i].Name)
	}
	return names
}

func deleteNodes(cluster *pb.K8Scluster, nodes map[string]int32) (*pb.K8Scluster, error) {
	master, worker := separateNodepools(nodes, cluster.ClusterInfo)
	newCluster, err := callDeleteNodes(master, worker, cluster)
	if err != nil {
		return nil, fmt.Errorf("error while deleting nodes for %s : %w", cluster.ClusterInfo.Name, err)
	}

	return newCluster, nil
}
