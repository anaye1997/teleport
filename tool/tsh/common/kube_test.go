/*
Copyright 2022 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/gravitational/trace"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib"
	"github.com/gravitational/teleport/lib/asciitable"
	kubeserver "github.com/gravitational/teleport/lib/kube/proxy/testing/kube_server"
	"github.com/gravitational/teleport/lib/modules"
	"github.com/gravitational/teleport/lib/service/servicecfg"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/teleport/tool/common"
)

func TestKube(t *testing.T) {
	lib.SetInsecureDevMode(true)
	t.Cleanup(func() { lib.SetInsecureDevMode(false) })

	pack := setupKubeTestPack(t)
	t.Run("list kube", pack.testListKube)
	t.Run("proxy kube", pack.testProxyKube)
}

type kubeTestPack struct {
	*suite

	rootClusterName  string
	leafClusterName  string
	rootKubeCluster1 string
	rootKubeCluster2 string
	leafKubeCluster  string
}

func setupKubeTestPack(t *testing.T) *kubeTestPack {
	t.Helper()

	ctx := context.Background()
	rootKubeCluster1 := "root-cluster"
	rootKubeCluster2 := "first-cluster"
	// mock a discovered kube cluster name in the leaf Teleport cluster.
	leafKubeCluster := "leaf-cluster-some-suffix-added-by-discovery-service"
	rootLabels := map[string]string{
		"label1": "val1",
		"ultra_long_label_for_teleport_kubernetes_service_list_kube_clusters_method": "ultra_long_label_value_for_teleport_kubernetes_service_list_kube_clusters_method",
	}
	leafLabels := map[string]string{
		"label1": "val1",
		"ultra_long_label_for_teleport_kubernetes_service_list_kube_clusters_method": "ultra_long_label_value_for_teleport_kubernetes_service_list_kube_clusters_method",
		// mock a discovered kube cluster in the leaf Teleport cluster.
		types.DiscoveredNameLabel: "leaf-cluster",
	}

	s := newTestSuite(t,
		withRootConfigFunc(func(cfg *servicecfg.Config) {
			cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
			cfg.Kube.Enabled = true
			cfg.Kube.ListenAddr = utils.MustParseAddr(localListenerAddr())
			cfg.Kube.KubeconfigPath = newKubeConfigFile(t, rootKubeCluster1, rootKubeCluster2)
			cfg.Kube.StaticLabels = rootLabels
		}),
		withLeafCluster(),
		withLeafConfigFunc(
			func(cfg *servicecfg.Config) {
				cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
				cfg.Kube.Enabled = true
				cfg.Kube.ListenAddr = utils.MustParseAddr(localListenerAddr())
				cfg.Kube.KubeconfigPath = newKubeConfigFile(t, leafKubeCluster)
				cfg.Kube.StaticLabels = leafLabels
			},
		),
		withValidationFunc(func(s *suite) bool {
			rootClusters, err := s.root.GetAuthServer().GetKubernetesServers(ctx)
			require.NoError(t, err)
			leafClusters, err := s.leaf.GetAuthServer().GetKubernetesServers(ctx)
			require.NoError(t, err)
			return len(rootClusters) >= 2 && len(leafClusters) >= 1
		}),
	)

	mustLoginSetEnv(t, s)
	return &kubeTestPack{
		suite:            s,
		rootClusterName:  s.root.Config.Auth.ClusterName.GetClusterName(),
		leafClusterName:  s.leaf.Config.Auth.ClusterName.GetClusterName(),
		rootKubeCluster1: rootKubeCluster1,
		rootKubeCluster2: rootKubeCluster2,
		leafKubeCluster:  leafKubeCluster,
	}
}

func (p *kubeTestPack) testListKube(t *testing.T) {
	staticRootLabels := p.suite.root.Config.Kube.StaticLabels
	formattedRootLabels := common.FormatLabels(staticRootLabels, false)
	formattedRootLabelsVerbose := common.FormatLabels(staticRootLabels, true)

	staticLeafLabels := p.suite.leaf.Config.Kube.StaticLabels
	formattedLeafLabels := common.FormatLabels(staticLeafLabels, false)
	formattedLeafLabelsVerbose := common.FormatLabels(staticLeafLabels, true)

	tests := []struct {
		name      string
		args      []string
		wantTable func() string
	}{
		{
			name: "default mode with truncated table",
			args: nil,
			wantTable: func() string {
				// p.rootKubeCluster2 ("first-cluster") should appear before
				// p.rootKubeCluster1 ("root-cluster") after sorting.
				table := asciitable.MakeTableWithTruncatedColumn(
					[]string{"Kube Cluster Name", "Labels", "Selected"},
					[][]string{
						{p.rootKubeCluster2, formattedRootLabels, ""},
						{p.rootKubeCluster1, formattedRootLabels, ""},
					},
					"Labels")
				return table.AsBuffer().String()
			},
		},
		{
			name: "show complete list of labels",
			args: []string{"--verbose"},
			wantTable: func() string {
				table := asciitable.MakeTable(
					[]string{"Kube Cluster Name", "Labels", "Selected"},
					[]string{p.rootKubeCluster2, formattedRootLabelsVerbose, ""},
					[]string{p.rootKubeCluster1, formattedRootLabelsVerbose, ""})
				return table.AsBuffer().String()
			},
		},
		{
			name: "show headless table",
			args: []string{"--quiet"},
			wantTable: func() string {
				table := asciitable.MakeHeadlessTable(2)
				table.AddRow([]string{p.rootKubeCluster2, formattedRootLabels, ""})
				table.AddRow([]string{p.rootKubeCluster1, formattedRootLabels, ""})

				return table.AsBuffer().String()
			},
		},
		{
			name: "list all clusters including leaf clusters",
			args: []string{"--all"},
			wantTable: func() string {
				table := asciitable.MakeTableWithTruncatedColumn(
					[]string{"Proxy", "Cluster", "Kube Cluster Name", "Labels"},
					[][]string{
						// "leaf-cluster" should be displayed instead of the
						// full leaf cluster name, since it is mocked as a
						// discovered resource and the discovered resource name
						// is displayed in non-verbose mode.
						{p.root.Config.Proxy.WebAddr.String(), "leaf1", "leaf-cluster", formattedLeafLabels},
						{p.root.Config.Proxy.WebAddr.String(), "root", p.rootKubeCluster2, formattedRootLabels},
						{p.root.Config.Proxy.WebAddr.String(), "root", p.rootKubeCluster1, formattedRootLabels},
					},
					"Labels",
				)
				return table.AsBuffer().String()
			},
		},
		{
			name: "list all clusters including leaf clusters with complete list of labels",
			args: []string{"--all", "--verbose"},
			wantTable: func() string {
				table := asciitable.MakeTable(
					[]string{"Proxy", "Cluster", "Kube Cluster Name", "Labels"},
					[]string{p.root.Config.Proxy.WebAddr.String(), "leaf1", p.leafKubeCluster, formattedLeafLabelsVerbose},
					[]string{p.root.Config.Proxy.WebAddr.String(), "root", p.rootKubeCluster2, formattedRootLabelsVerbose},
					[]string{p.root.Config.Proxy.WebAddr.String(), "root", p.rootKubeCluster1, formattedRootLabelsVerbose},
				)
				return table.AsBuffer().String()
			},
		},
		{
			name: "list all clusters including leaf clusters in headless table",
			args: []string{"--all", "--quiet"},
			wantTable: func() string {
				table := asciitable.MakeHeadlessTable(4)
				table.AddRow([]string{p.root.Config.Proxy.WebAddr.String(), "leaf1", "leaf-cluster", formattedLeafLabels})
				table.AddRow([]string{p.root.Config.Proxy.WebAddr.String(), "root", p.rootKubeCluster2, formattedRootLabels})
				table.AddRow([]string{p.root.Config.Proxy.WebAddr.String(), "root", p.rootKubeCluster1, formattedRootLabels})
				return table.AsBuffer().String()
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			captureStdout := new(bytes.Buffer)
			err := Run(
				context.Background(),
				append([]string{
					"--insecure",
					"kube",
					"ls",
				},
					tc.args...,
				),
				setCopyStdout(captureStdout),

				// set a custom empty kube config for each test, as we do
				// not want parallel (or even shuffled sequential) tests
				// potentially racing on the same config
				setKubeConfigPath(filepath.Join(t.TempDir(), "kubeconfig")),
			)
			require.NoError(t, err)
			got := strings.TrimSpace(captureStdout.String())
			want := strings.TrimSpace(tc.wantTable())
			diff := cmp.Diff(want, got)
			require.Empty(t, diff)
		})
	}
}

func TestKubeLoginAccessRequest(t *testing.T) {
	modules.SetTestModules(t,
		&modules.TestModules{
			TestBuildType: modules.BuildEnterprise,
			TestFeatures: modules.Features{
				Kubernetes: true,
			},
		},
	)
	lib.SetInsecureDevMode(true)
	t.Cleanup(func() { lib.SetInsecureDevMode(false) })

	const (
		roleName    = "requester"
		kubeCluster = "root-cluster"
	)
	// Create a role that allows the user to request access to the cluster but
	// not to access it directly.
	role, err := types.NewRole(
		roleName,
		types.RoleSpecV6{
			Allow: types.RoleConditions{
				Request: &types.AccessRequestConditions{
					SearchAsRoles: []string{"access"},
				},
			},
		},
	)
	require.NoError(t, err)

	s := newTestSuite(t,
		withRootConfigFunc(func(cfg *servicecfg.Config) {
			cfg.Auth.NetworkingConfig.SetProxyListenerMode(types.ProxyListenerMode_Multiplex)
			// reconfig the user to use the new role instead of the default ones
			// User is the second bootstrap resource.
			user, ok := cfg.Auth.BootstrapResources[1].(types.User)
			require.True(t, ok)
			user.SetRoles([]string{roleName})
			// Add the role to the list of bootstrap resources.
			cfg.Auth.BootstrapResources = append(cfg.Auth.BootstrapResources, role)

			// Enable kube and set the kubeconfig path.
			cfg.Kube.Enabled = true
			cfg.Kube.ListenAddr = utils.MustParseAddr(localListenerAddr())
			cfg.Kube.KubeconfigPath = newKubeConfigFile(t, kubeCluster)
		}),
		withValidationFunc(func(s *suite) bool {
			// Check if the kube cluster was added.
			rootClusters, err := s.root.GetAuthServer().GetKubernetesServers(context.Background())
			require.NoError(t, err)
			return len(rootClusters) == 1
		}),
	)
	// login as the user.
	tshHome, kubeConfig := mustLoginSetEnv(t, s)

	// Run the login command in a goroutine so we can check if the access
	// request was created and approved.
	// The goroutine will exit when the access request is approved.
	wg := &errgroup.Group{}
	wg.Go(func() error {
		err := Run(
			context.Background(),
			[]string{
				"--insecure",
				"kube",
				"login",
				kubeCluster,
				"--request-reason",
				"test",
			},
			setHomePath(tshHome),
			setKubeConfigPath(kubeConfig),
		)
		return trace.Wrap(err)
	})
	// Wait for the access request to be created and finally approve it.
	var accessRequestID string
	require.Eventually(t, func() bool {
		accessRequests, err := s.root.GetAuthServer().
			GetAccessRequests(
				context.Background(),
				types.AccessRequestFilter{State: types.RequestState_PENDING},
			)
		if err != nil || len(accessRequests) != 1 {
			return false
		}

		equal := reflect.DeepEqual(
			accessRequests[0].GetRequestedResourceIDs(),
			[]types.ResourceID{
				{
					ClusterName: s.root.Config.Auth.ClusterName.GetClusterName(),
					Kind:        types.KindKubernetesCluster,
					Name:        kubeCluster,
				},
			},
		)
		accessRequestID = accessRequests[0].GetName()

		return equal
	}, 10*time.Second, 1*time.Second)
	// Approve the access request to release the login command lock.
	err = s.root.GetAuthServer().SetAccessRequestState(context.Background(), types.AccessRequestUpdate{
		RequestID: accessRequestID,
		State:     types.RequestState_APPROVED,
	})
	require.NoError(t, err)
	// Wait for the login command to exit after the request is approved
	require.NoError(t, wg.Wait())
}

func newKubeConfigFile(t *testing.T, clusterNames ...string) string {
	tmpDir := t.TempDir()

	kubeConf := clientcmdapi.NewConfig()
	for _, name := range clusterNames {
		kubeConf.Clusters[name] = &clientcmdapi.Cluster{
			Server:                newKubeSelfSubjectServer(t),
			InsecureSkipTLSVerify: true,
		}
		kubeConf.AuthInfos[name] = &clientcmdapi.AuthInfo{}

		kubeConf.Contexts[name] = &clientcmdapi.Context{
			Cluster:  name,
			AuthInfo: name,
		}
	}
	kubeConfigLocation := filepath.Join(tmpDir, "kubeconfig")
	err := clientcmd.WriteToFile(*kubeConf, kubeConfigLocation)
	require.NoError(t, err)
	return kubeConfigLocation
}

func newKubeSelfSubjectServer(t *testing.T) string {
	srv, err := kubeserver.NewKubeAPIMock()
	require.NoError(t, err)
	t.Cleanup(func() { srv.Close() })

	return srv.URL
}
