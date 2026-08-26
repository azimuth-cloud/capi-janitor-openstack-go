package cleanup

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestClusterResourceSelectors(t *testing.T) {
	t.Parallel()

	t.Run("floating IP", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name        string
			description string
			want        bool
		}{
			{
				name:        "owned service address",
				description: "Floating IP for Kubernetes external service default/web from cluster demo",
				want:        true,
			},
			{
				name:        "other cluster",
				description: "Floating IP for Kubernetes external service default/web from cluster demo-2",
			},
			{
				name:        "broader Kubernetes prefix",
				description: "Floating IP for Kubernetes worker from cluster demo",
			},
			{
				name:        "wrong suffix",
				description: "Floating IP for Kubernetes external service default/web for cluster demo",
			},
			{name: "empty description"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got := floatingIPDescriptionMatchesCluster(FloatingIP{Description: test.description}, "demo")
				if got != test.want {
					t.Fatalf("floatingIPDescriptionMatchesCluster() = %t, want %t", got, test.want)
				}
			})
		}
	})

	t.Run("load balancer", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name             string
			loadBalancerName string
			want             bool
		}{
			{name: "owned service", loadBalancerName: "kube_service_demo_default_web", want: true},
			{name: "other cluster", loadBalancerName: "kube_service_demo2_default_web"},
			{name: "missing suffix", loadBalancerName: "kube_service_demo"},
			{name: "wrong prefix", loadBalancerName: "demo-kube-service-default-web"},
			{name: "API server", loadBalancerName: "demo-control-plane"},
			{name: "empty name"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got := loadBalancerNameMatchesCluster(LoadBalancer{Name: test.loadBalancerName}, "demo")
				if got != test.want {
					t.Fatalf("loadBalancerNameMatchesCluster() = %t, want %t", got, test.want)
				}
			})
		}
	})

	t.Run("security group", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name        string
			description string
			want        bool
		}{
			{
				name:        "owned service group",
				description: "Security Group for Service LoadBalancer in cluster demo",
				want:        true,
			},
			{
				name:        "other cluster",
				description: "Security Group for Service LoadBalancer in cluster demo-2",
			},
			{
				name:        "wrong prefix",
				description: "Group for Service LoadBalancer in cluster demo",
			},
			{
				name:        "wrong suffix",
				description: "Security Group for Service LoadBalancer for cluster demo",
			},
			{name: "empty description"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got := securityGroupDescriptionMatchesCluster(SecurityGroup{Description: test.description}, "demo")
				if got != test.want {
					t.Fatalf("securityGroupDescriptionMatchesCluster() = %t, want %t", got, test.want)
				}
			})
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			metadata map[string]string
			want     bool
		}{
			{name: "owned", metadata: map[string]string{cinderClusterMetadataKey: "demo"}, want: true},
			{name: "other cluster", metadata: map[string]string{cinderClusterMetadataKey: "demo-2"}},
			{name: "missing metadata"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got := snapshotMetadataMatchesCluster(Snapshot{Metadata: test.metadata}, "demo")
				if got != test.want {
					t.Fatalf("snapshotMetadataMatchesCluster() = %t, want %t", got, test.want)
				}
			})
		}
	})

	t.Run("volume", func(t *testing.T) {
		t.Parallel()
		tests := []struct {
			name     string
			metadata map[string]string
			want     bool
		}{
			{name: "owned", metadata: map[string]string{cinderClusterMetadataKey: "demo"}, want: true},
			{
				name: "exact keep value",
				metadata: map[string]string{
					cinderClusterMetadataKey: "demo",
					volumeKeepMetadataKey:    "true",
				},
			},
			{
				name: "different keep case",
				metadata: map[string]string{
					cinderClusterMetadataKey: "demo",
					volumeKeepMetadataKey:    "True",
				},
				want: true,
			},
			{
				name: "other keep value",
				metadata: map[string]string{
					cinderClusterMetadataKey: "demo",
					volumeKeepMetadataKey:    "yes",
				},
				want: true,
			},
			{name: "other cluster", metadata: map[string]string{cinderClusterMetadataKey: "demo-2"}},
			{name: "missing metadata"},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				got := volumeShouldBeDeletedForCluster(Volume{Metadata: test.metadata}, "demo")
				if got != test.want {
					t.Fatalf("volumeShouldBeDeletedForCluster() = %t, want %t", got, test.want)
				}
			})
		}
	})
}

func TestSelectLoadBalancersAndFloatingIPsForDeletion(t *testing.T) {
	t.Parallel()

	clusterFloatingIP := FloatingIP{
		ID:             "fip-owned",
		Description:    "Floating IP for Kubernetes external service default/web from cluster demo",
		AttachedPortID: "vip-1",
	}
	clusterLoadBalancer := LoadBalancer{
		ID:        "lb-owned",
		Name:      "kube_service_demo_default_web",
		VIPPortID: "vip-1",
	}

	tests := []struct {
		name                string
		loadBalancers       []LoadBalancer
		floatingIPs         []FloatingIP
		wantFloatingIPIDs   []string
		wantLoadBalancerIDs []string
		wantErr             bool
	}{
		{
			name:                "empty tags keep the legacy selector",
			loadBalancers:       []LoadBalancer{clusterLoadBalancer},
			floatingIPs:         []FloatingIP{clusterFloatingIP},
			wantFloatingIPIDs:   []string{"fip-owned"},
			wantLoadBalancerIDs: []string{"lb-owned"},
		},
		{
			name: "target cluster tags allow shared service deletion",
			loadBalancers: []LoadBalancer{{
				ID:        clusterLoadBalancer.ID,
				Name:      clusterLoadBalancer.Name,
				VIPPortID: clusterLoadBalancer.VIPPortID,
				Tags: []string{
					"kube_service_demo_default_web",
					"kube_service_demo_monitoring_ingress",
					"environment=production",
				},
			}},
			floatingIPs:         []FloatingIP{clusterFloatingIP},
			wantFloatingIPIDs:   []string{"fip-owned"},
			wantLoadBalancerIDs: []string{"lb-owned"},
		},
		{
			name: "foreign tag protects the load balancer and its floating IP",
			loadBalancers: []LoadBalancer{{
				ID:        clusterLoadBalancer.ID,
				Name:      clusterLoadBalancer.Name,
				VIPPortID: clusterLoadBalancer.VIPPortID,
				Tags:      []string{"kube_service_other_default_web"},
			}},
			floatingIPs: []FloatingIP{
				clusterFloatingIP,
				{
					ID:             "fip-unrelated",
					Description:    "Floating IP for Kubernetes external service default/api from cluster demo",
					AttachedPortID: "other-port",
				},
			},
			wantFloatingIPIDs: []string{"fip-unrelated"},
		},
		{
			name: "malformed reserved tag protects the load balancer",
			loadBalancers: []LoadBalancer{{
				ID:        clusterLoadBalancer.ID,
				Name:      clusterLoadBalancer.Name,
				VIPPortID: clusterLoadBalancer.VIPPortID,
				Tags:      []string{"kube_service_demo_"},
			}},
			floatingIPs: []FloatingIP{clusterFloatingIP},
		},
		{
			name: "tags do not discover a load balancer with a different name",
			loadBalancers: []LoadBalancer{{
				ID:        "lb-other",
				Name:      "kube_service_other_default_web",
				VIPPortID: "vip-1",
				Tags:      []string{"kube_service_demo_default_web"},
			}},
			floatingIPs:       []FloatingIP{clusterFloatingIP},
			wantFloatingIPIDs: []string{"fip-owned"},
		},
		{
			name: "protected load balancer requires a VIP port",
			loadBalancers: []LoadBalancer{{
				ID:   clusterLoadBalancer.ID,
				Name: clusterLoadBalancer.Name,
				Tags: []string{"kube_service_other_default_web"},
			}},
			floatingIPs: []FloatingIP{clusterFloatingIP},
			wantErr:     true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectLoadBalancersAndFloatingIPsForDeletion("demo", test.loadBalancers, test.floatingIPs)
			if (err != nil) != test.wantErr {
				t.Fatalf("selectLoadBalancersAndFloatingIPsForDeletion() error = %v, want error %t", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if !slices.Equal(got.floatingIPIDsToDelete, test.wantFloatingIPIDs) {
				t.Fatalf("floating IP IDs = %v, want %v", got.floatingIPIDsToDelete, test.wantFloatingIPIDs)
			}
			if !slices.Equal(got.loadBalancerIDsToDelete, test.wantLoadBalancerIDs) {
				t.Fatalf("load balancer IDs = %v, want %v", got.loadBalancerIDsToDelete, test.wantLoadBalancerIDs)
			}
		})
	}
}

func TestRunnerProcessesOnlyEarliestRemainingPhase(t *testing.T) {
	t.Parallel()

	clusterFloatingIP := FloatingIP{
		ID:          "fip-1",
		Description: "Floating IP for Kubernetes external service default/web from cluster demo",
	}
	clusterLoadBalancer := LoadBalancer{ID: "lb-1", Name: "kube_service_demo_default_web"}
	clusterSecurityGroup := SecurityGroup{ID: "sg-1", Description: "Security Group for Service LoadBalancer in cluster demo"}
	clusterSnapshot := Snapshot{ID: "snapshot-1", Metadata: map[string]string{cinderClusterMetadataKey: "demo"}}
	clusterVolume := Volume{ID: "volume-1", Metadata: map[string]string{cinderClusterMetadataKey: "demo"}}

	tests := []struct {
		name              string
		configureServices func(*fakeResourceServices)
		deleteVolumes     bool
		wantCalls         []string
		wantOutcome       Outcome
	}{
		{
			name: "floating IP before load balancer",
			configureServices: func(services *fakeResourceServices) {
				services.floatingIPs = []FloatingIP{clusterFloatingIP}
				services.loadBalancers = []LoadBalancer{clusterLoadBalancer}
			},
			wantCalls:   []string{"list:load-balancers", "list:floating-ips", "delete:floating-ip:fip-1"},
			wantOutcome: OutcomeWaiting,
		},
		{
			name: "load balancer before security group",
			configureServices: func(services *fakeResourceServices) {
				services.loadBalancers = []LoadBalancer{clusterLoadBalancer}
				services.securityGroups = []SecurityGroup{clusterSecurityGroup}
			},
			wantCalls:   []string{"list:load-balancers", "list:floating-ips", "delete:load-balancer:lb-1"},
			wantOutcome: OutcomeWaiting,
		},
		{
			name: "security group before snapshot",
			configureServices: func(services *fakeResourceServices) {
				services.securityGroups = []SecurityGroup{clusterSecurityGroup}
				services.snapshots = []Snapshot{clusterSnapshot}
			},
			deleteVolumes: true,
			wantCalls: []string{
				"list:load-balancers",
				"list:floating-ips",
				"list:security-groups",
				"delete:security-group:sg-1",
			},
			wantOutcome: OutcomeWaiting,
		},
		{
			name: "snapshot before volume",
			configureServices: func(services *fakeResourceServices) {
				services.snapshots = []Snapshot{clusterSnapshot}
				services.volumes = []Volume{clusterVolume}
			},
			deleteVolumes: true,
			wantCalls: []string{
				"list:load-balancers",
				"list:floating-ips",
				"list:security-groups",
				"list:snapshots",
				"delete:snapshot:snapshot-1",
			},
			wantOutcome: OutcomeWaiting,
		},
		{
			name: "volume after empty snapshot phase",
			configureServices: func(services *fakeResourceServices) {
				services.volumes = []Volume{clusterVolume}
			},
			deleteVolumes: true,
			wantCalls: []string{
				"list:load-balancers",
				"list:floating-ips",
				"list:security-groups",
				"list:snapshots",
				"list:volumes",
				"delete:volume:volume-1",
			},
			wantOutcome: OutcomeWaiting,
		},
		{
			name:              "complete inventory",
			configureServices: func(*fakeResourceServices) {},
			deleteVolumes:     true,
			wantCalls: []string{
				"list:load-balancers",
				"list:floating-ips",
				"list:security-groups",
				"list:snapshots",
				"list:volumes",
			},
			wantOutcome: OutcomeComplete,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			services := &fakeResourceServices{}
			test.configureServices(services)
			runner := newRunnerWithFakeServices(services)
			result, err := runner.Run(context.Background(), Request{
				Scope:  Scope{ClusterName: "demo"},
				Policy: Policy{DeleteVolumes: test.deleteVolumes},
			})
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if result.Outcome != test.wantOutcome {
				t.Fatalf("Run() outcome = %q, want %q", result.Outcome, test.wantOutcome)
			}
			if !reflect.DeepEqual(services.recordedCalls, test.wantCalls) {
				t.Fatalf("calls = %v, want %v", services.recordedCalls, test.wantCalls)
			}
		})
	}
}

func TestRunnerObservesAbsenceBeforeAdvancing(t *testing.T) {
	t.Parallel()

	services := &fakeResourceServices{
		floatingIPs: []FloatingIP{{
			ID:          "fip-1",
			Description: "Floating IP for Kubernetes external service default/web from cluster demo",
		}},
		loadBalancers: []LoadBalancer{{ID: "lb-1", Name: "kube_service_demo_default_web"}},
	}
	runner := newRunnerWithFakeServices(services)
	request := Request{Scope: Scope{ClusterName: "demo"}}

	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if result.Outcome != OutcomeWaiting {
		t.Fatalf("first Run() outcome = %q, want %q", result.Outcome, OutcomeWaiting)
	}
	if !reflect.DeepEqual(services.recordedCalls, []string{
		"list:load-balancers",
		"list:floating-ips",
		"delete:floating-ip:fip-1",
	}) {
		t.Fatalf("first calls = %v", services.recordedCalls)
	}

	services.recordedCalls = nil
	services.floatingIPs = nil
	result, err = runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if result.Outcome != OutcomeWaiting {
		t.Fatalf("second Run() outcome = %q, want %q", result.Outcome, OutcomeWaiting)
	}
	if !reflect.DeepEqual(services.recordedCalls, []string{
		"list:load-balancers",
		"list:floating-ips",
		"delete:load-balancer:lb-1",
	}) {
		t.Fatalf("second calls = %v", services.recordedCalls)
	}
}

func TestRunnerSkipsVolumeServicesWhenDeletionIsDisabled(t *testing.T) {
	t.Parallel()

	services := &fakeResourceServices{}
	runner := NewRunner(Services{
		FloatingIPs:    services,
		LoadBalancers:  services,
		SecurityGroups: services,
	})
	result, err := runner.Run(context.Background(), Request{Scope: Scope{ClusterName: "demo"}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome != OutcomeComplete {
		t.Fatalf("Run() outcome = %q, want %q", result.Outcome, OutcomeComplete)
	}
	if !reflect.DeepEqual(services.recordedCalls, []string{
		"list:load-balancers",
		"list:floating-ips",
		"list:security-groups",
	}) {
		t.Fatalf("calls = %v", services.recordedCalls)
	}
}

func TestRunnerHandlesPhaseErrors(t *testing.T) {
	t.Parallel()

	t.Run("pending delete continues within the current phase", func(t *testing.T) {
		t.Parallel()
		services := &fakeResourceServices{
			floatingIPs: []FloatingIP{
				{
					ID:          "fip-pending",
					Description: "Floating IP for Kubernetes external service default/one from cluster demo",
				},
				{
					ID:          "fip-accepted",
					Description: "Floating IP for Kubernetes external service default/two from cluster demo",
				},
			},
			deleteResourceErrors: map[string]error{"floating-ip:fip-pending": ErrDeletePending},
		}
		result, err := newRunnerWithFakeServices(services).Run(
			context.Background(),
			Request{Scope: Scope{ClusterName: "demo"}},
		)
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if result.Outcome != OutcomeWaiting {
			t.Fatalf("Run() outcome = %q, want %q", result.Outcome, OutcomeWaiting)
		}
		if !reflect.DeepEqual(services.recordedCalls, []string{
			"list:load-balancers",
			"list:floating-ips",
			"delete:floating-ip:fip-pending",
			"delete:floating-ip:fip-accepted",
		}) {
			t.Fatalf("calls = %v", services.recordedCalls)
		}
	})

	t.Run("fatal delete stops the phase", func(t *testing.T) {
		t.Parallel()
		cause := errors.New("delete failed")
		services := &fakeResourceServices{
			floatingIPs: []FloatingIP{
				{
					ID:          "fip-failed",
					Description: "Floating IP for Kubernetes external service default/one from cluster demo",
				},
				{
					ID:          "fip-not-tried",
					Description: "Floating IP for Kubernetes external service default/two from cluster demo",
				},
			},
			deleteResourceErrors: map[string]error{"floating-ip:fip-failed": cause},
		}
		result, err := newRunnerWithFakeServices(services).Run(
			context.Background(),
			Request{Scope: Scope{ClusterName: "demo"}},
		)
		if !errors.Is(err, cause) {
			t.Fatalf("Run() error = %v, want cause %v", err, cause)
		}
		if result != (Result{}) {
			t.Fatalf("Run() result = %#v, want zero result", result)
		}
		if !reflect.DeepEqual(services.recordedCalls, []string{
			"list:load-balancers",
			"list:floating-ips",
			"delete:floating-ip:fip-failed",
		}) {
			t.Fatalf("calls = %v", services.recordedCalls)
		}
	})

	t.Run("empty selected ID blocks the phase before mutation", func(t *testing.T) {
		t.Parallel()
		services := &fakeResourceServices{
			floatingIPs: []FloatingIP{
				{
					ID:          "fip-valid",
					Description: "Floating IP for Kubernetes external service default/one from cluster demo",
				},
				{
					Description: "Floating IP for Kubernetes external service default/two from cluster demo",
				},
			},
		}
		result, err := newRunnerWithFakeServices(services).Run(
			context.Background(),
			Request{Scope: Scope{ClusterName: "demo"}},
		)
		if err == nil || !strings.Contains(err.Error(), "selected resource ID is empty") {
			t.Fatalf("Run() error = %v, want empty ID error", err)
		}
		if result != (Result{}) {
			t.Fatalf("Run() result = %#v, want zero result", result)
		}
		if !reflect.DeepEqual(services.recordedCalls, []string{"list:load-balancers", "list:floating-ips"}) {
			t.Fatalf("calls = %v", services.recordedCalls)
		}
	})

	t.Run("empty load balancer ID blocks floating IP mutation", func(t *testing.T) {
		t.Parallel()
		services := &fakeResourceServices{
			loadBalancers: []LoadBalancer{{Name: "kube_service_demo_default_web"}},
			floatingIPs: []FloatingIP{{
				ID:          "fip-valid",
				Description: "Floating IP for Kubernetes external service default/web from cluster demo",
			}},
		}
		result, err := newRunnerWithFakeServices(services).Run(
			context.Background(),
			Request{Scope: Scope{ClusterName: "demo"}},
		)
		if err == nil || !strings.Contains(err.Error(), "selected resource ID is empty") {
			t.Fatalf("Run() error = %v, want empty ID error", err)
		}
		if result != (Result{}) {
			t.Fatalf("Run() result = %#v, want zero result", result)
		}
		if !reflect.DeepEqual(services.recordedCalls, []string{"list:load-balancers", "list:floating-ips"}) {
			t.Fatalf("calls = %v", services.recordedCalls)
		}
	})

	t.Run("protected load balancer without VIP blocks all mutation", func(t *testing.T) {
		t.Parallel()
		services := &fakeResourceServices{
			loadBalancers: []LoadBalancer{{
				ID:   "lb-shared",
				Name: "kube_service_demo_default_web",
				Tags: []string{"kube_service_other_default_web"},
			}},
			floatingIPs: []FloatingIP{{
				ID:          "fip-owned",
				Description: "Floating IP for Kubernetes external service default/web from cluster demo",
			}},
		}
		result, err := newRunnerWithFakeServices(services).Run(
			context.Background(),
			Request{Scope: Scope{ClusterName: "demo"}},
		)
		if err == nil {
			t.Fatal("Run() error = nil, want incomplete load balancer ownership error")
		}
		if result != (Result{}) {
			t.Fatalf("Run() result = %#v, want zero result", result)
		}
		if !reflect.DeepEqual(services.recordedCalls, []string{"list:load-balancers", "list:floating-ips"}) {
			t.Fatalf("calls = %v", services.recordedCalls)
		}
	})
}

func TestRunnerListErrorsStopBeforeMutation(t *testing.T) {
	t.Parallel()

	cause := errors.New("incomplete inventory")
	tests := []struct {
		name              string
		configureServices func(*fakeResourceServices)
		deleteVolumes     bool
		wantCalls         []string
	}{
		{
			name: "load balancers",
			configureServices: func(services *fakeResourceServices) {
				services.listLoadBalancersErr = cause
			},
			wantCalls: []string{"list:load-balancers"},
		},
		{
			name: "floating IPs",
			configureServices: func(services *fakeResourceServices) {
				services.listFloatingIPsErr = cause
			},
			wantCalls: []string{"list:load-balancers", "list:floating-ips"},
		},
		{
			name: "security groups",
			configureServices: func(services *fakeResourceServices) {
				services.listSecurityGroupsErr = cause
			},
			wantCalls: []string{"list:load-balancers", "list:floating-ips", "list:security-groups"},
		},
		{
			name: "snapshots",
			configureServices: func(services *fakeResourceServices) {
				services.listSnapshotsErr = cause
			},
			deleteVolumes: true,
			wantCalls: []string{
				"list:load-balancers",
				"list:floating-ips",
				"list:security-groups",
				"list:snapshots",
			},
		},
		{
			name: "volumes",
			configureServices: func(services *fakeResourceServices) {
				services.listVolumesErr = cause
			},
			deleteVolumes: true,
			wantCalls: []string{
				"list:load-balancers",
				"list:floating-ips",
				"list:security-groups",
				"list:snapshots",
				"list:volumes",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			services := &fakeResourceServices{}
			test.configureServices(services)
			result, err := newRunnerWithFakeServices(services).Run(context.Background(), Request{
				Scope:  Scope{ClusterName: "demo"},
				Policy: Policy{DeleteVolumes: test.deleteVolumes},
			})
			if !errors.Is(err, cause) {
				t.Fatalf("Run() error = %v, want cause %v", err, cause)
			}
			if result != (Result{}) {
				t.Fatalf("Run() result = %#v, want zero result", result)
			}
			if !reflect.DeepEqual(services.recordedCalls, test.wantCalls) {
				t.Fatalf("calls = %v, want %v", services.recordedCalls, test.wantCalls)
			}
		})
	}
}

type fakeResourceServices struct {
	floatingIPs           []FloatingIP
	loadBalancers         []LoadBalancer
	securityGroups        []SecurityGroup
	snapshots             []Snapshot
	volumes               []Volume
	listFloatingIPsErr    error
	listLoadBalancersErr  error
	listSecurityGroupsErr error
	listSnapshotsErr      error
	listVolumesErr        error
	deleteResourceErrors  map[string]error
	recordedCalls         []string
}

func (s *fakeResourceServices) ListFloatingIPs(context.Context) ([]FloatingIP, error) {
	s.recordedCalls = append(s.recordedCalls, "list:floating-ips")
	return s.floatingIPs, s.listFloatingIPsErr
}

func (s *fakeResourceServices) DeleteFloatingIP(_ context.Context, id string) error {
	return s.recordDeleteCall("floating-ip", id)
}

func (s *fakeResourceServices) ListLoadBalancers(context.Context) ([]LoadBalancer, error) {
	s.recordedCalls = append(s.recordedCalls, "list:load-balancers")
	return s.loadBalancers, s.listLoadBalancersErr
}

func (s *fakeResourceServices) DeleteLoadBalancer(_ context.Context, id string) error {
	return s.recordDeleteCall("load-balancer", id)
}

func (s *fakeResourceServices) ListSecurityGroups(context.Context) ([]SecurityGroup, error) {
	s.recordedCalls = append(s.recordedCalls, "list:security-groups")
	return s.securityGroups, s.listSecurityGroupsErr
}

func (s *fakeResourceServices) DeleteSecurityGroup(_ context.Context, id string) error {
	return s.recordDeleteCall("security-group", id)
}

func (s *fakeResourceServices) ListSnapshots(context.Context) ([]Snapshot, error) {
	s.recordedCalls = append(s.recordedCalls, "list:snapshots")
	return s.snapshots, s.listSnapshotsErr
}

func (s *fakeResourceServices) DeleteSnapshot(_ context.Context, id string) error {
	return s.recordDeleteCall("snapshot", id)
}

func (s *fakeResourceServices) ListVolumes(context.Context) ([]Volume, error) {
	s.recordedCalls = append(s.recordedCalls, "list:volumes")
	return s.volumes, s.listVolumesErr
}

func (s *fakeResourceServices) DeleteVolume(_ context.Context, id string) error {
	return s.recordDeleteCall("volume", id)
}

func (s *fakeResourceServices) recordDeleteCall(resourceKind, resourceID string) error {
	resourceKey := resourceKind + ":" + resourceID
	s.recordedCalls = append(s.recordedCalls, "delete:"+resourceKey)
	return s.deleteResourceErrors[resourceKey]
}

func newRunnerWithFakeServices(services *fakeResourceServices) Runner {
	return NewRunner(Services{
		FloatingIPs:    services,
		LoadBalancers:  services,
		SecurityGroups: services,
		Snapshots:      services,
		Volumes:        services,
	})
}
