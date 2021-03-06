// Package telemtry contains telemetry provider implementations as well as common code that can be
// shared by each telemetry vendor.  Istio vendor is the canonical impl.
package telemetry

import (
	"fmt"
	"time"

	"github.com/kiali/kiali/graph"
	"github.com/kiali/kiali/log"
)

// MergeTrafficMaps typically combines two namespace traffic maps. It ensures that we only
// have unique nodes by removing duplicate nodes and merging their edges.  When removing a
// duplicate prefer an instance from the namespace being merged-in because it is guaranteed
// to have all appender information applied (i.e. not an outsider). We also need to avoid duplicate
// edges, it can happen when a terminal node of one namespace is a root node of another:
//   ns1 graph: unknown -> ns1:A -> ns2:B
//   ns2 graph:   ns1:A -> ns2:B -> ns2:C
func MergeTrafficMaps(trafficMap graph.TrafficMap, ns string, nsTrafficMap graph.TrafficMap) {
	for nsID, nsNode := range nsTrafficMap {
		if node, isDup := trafficMap[nsID]; isDup {
			if nsNode.Namespace == ns {
				// prefer nsNode (see above comment), so do a swap
				trafficMap[nsID] = nsNode
				temp := node
				node = nsNode
				nsNode = temp
			}
			for _, nsEdge := range nsNode.Edges {
				isDupEdge := false
				for _, e := range node.Edges {
					if nsEdge.Dest.ID == e.Dest.ID && nsEdge.Metadata[graph.ProtocolKey] == e.Metadata[graph.ProtocolKey] {
						isDupEdge = true
						break
					}
				}
				if !isDupEdge {
					node.Edges = append(node.Edges, nsEdge)
					// add traffic for the new edge
					graph.AddOutgoingEdgeToMetadata(node.Metadata, nsEdge.Metadata)
				}
			}
		} else {
			trafficMap[nsID] = nsNode
		}
	}
}

// MarkOutsideOrInaccessible sets metadata for outsider and inaccessible nodes.  It should be called
// after all appender work is completed.
func MarkOutsideOrInaccessible(trafficMap graph.TrafficMap, o graph.TelemetryOptions) {
	for _, n := range trafficMap {
		switch n.NodeType {
		case graph.NodeTypeUnknown:
			n.Metadata[graph.IsInaccessible] = true
		case graph.NodeTypeService:
			if n.Namespace == graph.Unknown && n.Service == graph.Unknown {
				n.Metadata[graph.IsInaccessible] = true
			} else if n.Metadata[graph.IsEgressCluster] == true {
				n.Metadata[graph.IsInaccessible] = true
			} else {
				if isOutside(n, o.Namespaces) {
					n.Metadata[graph.IsOutside] = true
				}
			}
		default:
			if isOutside(n, o.Namespaces) {
				n.Metadata[graph.IsOutside] = true
			}
		}
		if isOutsider, ok := n.Metadata[graph.IsOutside]; ok && isOutsider.(bool) {
			if _, ok2 := n.Metadata[graph.IsInaccessible]; !ok2 {
				if isInaccessible(n, o.AccessibleNamespaces) {
					n.Metadata[graph.IsInaccessible] = true
				}
			}
		}
	}
}

func isOutside(n *graph.Node, namespaces map[string]graph.NamespaceInfo) bool {
	if n.Namespace == graph.Unknown {
		return false
	}
	for _, ns := range namespaces {
		if n.Namespace == ns.Name {
			return false
		}
	}
	return true
}

func isInaccessible(n *graph.Node, accessibleNamespaces map[string]time.Time) bool {
	if _, found := accessibleNamespaces[n.Namespace]; !found {
		return true
	} else {
		return false
	}
}

// MarkTrafficGenerators set IsRoot metadata. It is called after appender work is complete.
func MarkTrafficGenerators(trafficMap graph.TrafficMap) {
	destMap := make(map[string]*graph.Node)
	for _, n := range trafficMap {
		for _, e := range n.Edges {
			destMap[e.Dest.ID] = e.Dest
		}
	}
	for _, n := range trafficMap {
		if len(n.Edges) == 0 {
			continue
		}
		if _, isDest := destMap[n.ID]; !isDest {
			n.Metadata[graph.IsRoot] = true
		}
	}
}

// ReduceToServiceGraph compresses a [service-injected workload] graph by removing
// the workload nodes such that, with exception of non-service root nodes, the resulting
// graph has edges only from and to service nodes.  It is typically the last thing called
// prior to retruning the service graph.
func ReduceToServiceGraph(trafficMap graph.TrafficMap) graph.TrafficMap {
	reducedTrafficMap := graph.NewTrafficMap()

	for id, n := range trafficMap {
		if n.NodeType != graph.NodeTypeService {
			// if node isRoot then keep it to better understand traffic flow.
			if val, ok := n.Metadata[graph.IsRoot]; ok && val.(bool) {
				// Remove any edge to a non-service node.  The service graph only shows non-service root
				// nodes, all other nodes are service nodes.  The use case is direct workload-to-workload
				// traffic, which is unusual but possible.  This can lead to nodes with outgoing traffic
				// not represented by an outgoing edge, but that is the nature of the graph type.
				serviceEdges := []*graph.Edge{}
				for _, e := range n.Edges {
					if e.Dest.NodeType == graph.NodeTypeService {
						serviceEdges = append(serviceEdges, e)
					} else {
						log.Tracef("Service graph ignoring non-service root destination [%s]", e.Dest.Workload)
					}
				}
				n.Edges = serviceEdges
				reducedTrafficMap[id] = n
			}
			continue
		}

		// handle service node, add to reduced traffic map and generate new edges
		reducedTrafficMap[id] = n
		workloadEdges := n.Edges
		n.Edges = []*graph.Edge{}
		for _, workloadEdge := range workloadEdges {
			workload := workloadEdge.Dest
			checkNodeType(graph.NodeTypeWorkload, workload)
			for _, serviceEdge := range workload.Edges {
				// As above, ignore edges to non-service destinations
				if serviceEdge.Dest.NodeType != graph.NodeTypeService {
					log.Tracef("Service graph ignoring non-service destination [%s]", serviceEdge.Dest.Workload)
					continue
				}
				childService := serviceEdge.Dest
				var edge *graph.Edge
				for _, e := range n.Edges {
					if childService.ID == e.Dest.ID && serviceEdge.Metadata[graph.ProtocolKey] == e.Metadata[graph.ProtocolKey] {
						edge = e
						break
					}
				}
				if nil == edge {
					n.Edges = append(n.Edges, serviceEdge)
				} else {
					addServiceGraphTraffic(edge, serviceEdge)
				}
			}
		}
	}

	return reducedTrafficMap
}

func addServiceGraphTraffic(toEdge, fromEdge *graph.Edge) {
	graph.AggregateEdgeTraffic(fromEdge, toEdge)

	// handle any appender-based edge data (nothing currently)
	// note: We used to average response times of the aggregated edges but realized that
	// we can't average quantiles (kiali-2297).
}

func checkNodeType(expected string, n *graph.Node) {
	if expected != n.NodeType {
		graph.Error(fmt.Sprintf("Expected nodeType [%s] for node [%+v]", expected, n))
	}
}
