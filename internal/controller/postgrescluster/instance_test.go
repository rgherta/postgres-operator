// +build envtest

/*
 Copyright 2021 Crunchy Data Solutions, Inc.
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

package postgrescluster

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"gotest.tools/v3/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crunchydata/postgres-operator/internal/naming"
	"github.com/crunchydata/postgres-operator/pkg/apis/postgres-operator.crunchydata.com/v1beta1"
)

func TestInstanceIsRunning(t *testing.T) {
	var instance Instance
	var known, running bool

	// No pods
	running, known = instance.IsRunning("any")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// No statuses
	instance.Pods = []*corev1.Pod{{}}
	running, known = instance.IsRunning("any")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// No states
	instance.Pods[0].Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "c1",
	}}
	running, known = instance.IsRunning("c1")
	assert.Assert(t, known)
	assert.Assert(t, !running)

	running, known = instance.IsRunning("missing")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// Running state
	// - https://releases.k8s.io/v1.21.0/staging/src/k8s.io/kubectl/pkg/cmd/debug/debug.go#L668
	instance.Pods[0].Status.ContainerStatuses[0].State.Running =
		new(corev1.ContainerStateRunning)

	running, known = instance.IsRunning("c1")
	assert.Assert(t, known)
	assert.Assert(t, running)

	running, known = instance.IsRunning("missing")
	assert.Assert(t, !known)
	assert.Assert(t, !running)

	// Init containers
	instance.Pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "i1",
		State: corev1.ContainerState{
			Running: new(corev1.ContainerStateRunning),
		},
	}}

	running, known = instance.IsRunning("i1")
	assert.Assert(t, known)
	assert.Assert(t, running)
}

func TestNewObservedInstances(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := newObservedInstances(cluster, nil, nil)

		assert.Equal(t, len(observed.forCluster), 0)
		assert.Equal(t, len(observed.byName), 0)
		assert.Equal(t, len(observed.bySet), 0)
	})

	t.Run("PodMissingOthers", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := newObservedInstances(
			cluster,
			nil,
			[]corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "some-pod-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "missing",
							"postgres-operator.crunchydata.com/instance":     "the-name",
						},
					},
				},
			})

		// Registers as an instance.
		assert.Equal(t, len(observed.forCluster), 1)
		assert.Equal(t, len(observed.byName), 1)
		assert.Equal(t, len(observed.bySet), 1)

		instance := observed.forCluster[0]
		assert.Equal(t, instance.Name, "the-name")
		assert.Equal(t, len(instance.Pods), 1)   // The Pod
		assert.Assert(t, instance.Runner == nil) // No matching StatefulSet
		assert.Assert(t, instance.Spec == nil)   // No matching PostgresInstanceSetSpec

		// Lookup based on its labels.
		assert.Equal(t, observed.byName["the-name"], instance)
		assert.DeepEqual(t, observed.bySet["missing"], []*Instance{instance})
		assert.DeepEqual(t, observed.setNames.List(), []string{"missing"})
	})

	t.Run("RunnerMissingOthers", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		observed := newObservedInstances(
			cluster,
			[]appsv1.StatefulSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "the-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "missing",
						},
					},
				},
			},
			nil)

		// Registers as an instance.
		assert.Equal(t, len(observed.forCluster), 1)
		assert.Equal(t, len(observed.byName), 1)
		assert.Equal(t, len(observed.bySet), 1)

		instance := observed.forCluster[0]
		assert.Equal(t, instance.Name, "the-name")
		assert.Equal(t, len(instance.Pods), 0)   // No matching Pods
		assert.Assert(t, instance.Runner != nil) // The StatefulSet
		assert.Assert(t, instance.Spec == nil)   // No matching PostgresInstanceSetSpec

		// Lookup based on its name and labels.
		assert.Equal(t, observed.byName["the-name"], instance)
		assert.DeepEqual(t, observed.bySet["missing"], []*Instance{instance})
		assert.DeepEqual(t, observed.setNames.List(), []string{"missing"})
	})

	t.Run("Matching", func(t *testing.T) {
		cluster := new(v1beta1.PostgresCluster)
		cluster.Spec.InstanceSets = []v1beta1.PostgresInstanceSetSpec{{Name: "00"}}

		observed := newObservedInstances(
			cluster,
			[]appsv1.StatefulSet{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "the-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "00",
						},
					},
				},
			},
			[]corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "some-pod-name",
						Labels: map[string]string{
							"postgres-operator.crunchydata.com/instance-set": "00",
							"postgres-operator.crunchydata.com/instance":     "the-name",
						},
					},
				},
			})

		// Registers as one instance.
		assert.Equal(t, len(observed.forCluster), 1)
		assert.Equal(t, len(observed.byName), 1)
		assert.Equal(t, len(observed.bySet), 1)

		instance := observed.forCluster[0]
		assert.Equal(t, instance.Name, "the-name")
		assert.Equal(t, len(instance.Pods), 1)   // The Pod
		assert.Assert(t, instance.Runner != nil) // The StatefulSet
		assert.Assert(t, instance.Spec != nil)   // The PostgresInstanceSetSpec

		// Lookup based on its name and labels.
		assert.Equal(t, observed.byName["the-name"], instance)
		assert.DeepEqual(t, observed.bySet["00"], []*Instance{instance})
		assert.DeepEqual(t, observed.setNames.List(), []string{"00"})
	})
}

func TestAddPGBackRestToInstancePodSpec(t *testing.T) {

	clusterName := "hippo"
	clusterUID := types.UID("hippouid")
	namespace := "test-add-pgbackrest-to-instance-pod-spec"

	// create a PostgresCluster to test with
	postgresCluster := &v1beta1.PostgresCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
			UID:       clusterUID,
		},
		Spec: v1beta1.PostgresClusterSpec{
			Archive: v1beta1.Archive{
				PGBackRest: v1beta1.PGBackRestArchive{
					Repos: []v1beta1.PGBackRestRepo{{
						Name: "repo1",
						Volume: &v1beta1.RepoPVC{
							VolumeClaimSpec: v1.PersistentVolumeClaimSpec{
								AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
								Resources: v1.ResourceRequirements{
									Requests: map[v1.ResourceName]resource.Quantity{
										v1.ResourceStorage: resource.MustParse("1Gi"),
									},
								},
							},
						},
					}, {
						Name: "repo2",
						Volume: &v1beta1.RepoPVC{
							VolumeClaimSpec: v1.PersistentVolumeClaimSpec{
								AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteMany},
								Resources: v1.ResourceRequirements{
									Requests: map[v1.ResourceName]resource.Quantity{
										v1.ResourceStorage: resource.MustParse("2Gi"),
									},
								},
							},
						},
					}},
				},
			},
		},
	}

	instance := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "hippo-instance-abc",
			Namespace: namespace,
		},
	}

	testCases := []struct {
		repoHost  *v1beta1.PGBackRestRepoHost
		sshConfig *v1.ConfigMapProjection
		sshSecret *v1.SecretProjection
	}{{
		repoHost: nil,
	}, {
		repoHost: &v1beta1.PGBackRestRepoHost{},
	}, {
		repoHost: &v1beta1.PGBackRestRepoHost{
			Dedicated: &v1beta1.DedicatedRepo{
				Resources: &v1.ResourceRequirements{},
			},
		},
	}, {
		repoHost: nil,
		sshConfig: &v1.ConfigMapProjection{
			LocalObjectReference: v1.LocalObjectReference{Name: "cust-ssh-config.conf"}},
		sshSecret: &v1.SecretProjection{
			LocalObjectReference: v1.LocalObjectReference{Name: "cust-ssh-secret.conf"}},
	}, {
		repoHost: &v1beta1.PGBackRestRepoHost{},
		sshConfig: &v1.ConfigMapProjection{
			LocalObjectReference: v1.LocalObjectReference{Name: "cust-ssh-config.conf"}},
		sshSecret: &v1.SecretProjection{
			LocalObjectReference: v1.LocalObjectReference{Name: "cust-ssh-secret.conf"}},
	}, {
		repoHost: &v1beta1.PGBackRestRepoHost{
			Dedicated: &v1beta1.DedicatedRepo{
				Resources: &v1.ResourceRequirements{},
			},
		},
		sshConfig: &v1.ConfigMapProjection{
			LocalObjectReference: v1.LocalObjectReference{Name: "cust-ssh-config.conf"}},
		sshSecret: &v1.SecretProjection{
			LocalObjectReference: v1.LocalObjectReference{Name: "cust-ssh-secret.conf"}},
	}}

	for _, tc := range testCases {
		repoHost := (tc.repoHost != nil)
		dedicated := (tc.repoHost != nil && tc.repoHost.Dedicated != nil)
		customConfig := (tc.sshConfig != nil)
		customSecret := (tc.sshSecret != nil)
		t.Run(fmt.Sprintf("repoHost:%t, dedicated:%t", repoHost, dedicated), func(t *testing.T) {

			postgresCluster.Spec.Archive.PGBackRest.RepoHost = tc.repoHost
			template := &v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Containers: []v1.Container{{Name: naming.ContainerDatabase}},
				},
			}

			if repoHost {
				if customConfig {
					postgresCluster.Spec.Archive.PGBackRest.RepoHost.SSHConfiguration = tc.sshConfig
				}
				if customSecret {
					postgresCluster.Spec.Archive.PGBackRest.RepoHost.SSHSecret = tc.sshSecret
				}
			}

			err := addPGBackRestToInstancePodSpec(postgresCluster, template, instance)
			assert.NilError(t, err)

			// if there is no dedicated repo host configured, verfiy pgBackRest repos are mounted to the
			// instance Pod
			if !dedicated {
				for _, r := range postgresCluster.Spec.Archive.PGBackRest.Repos {
					var foundRepo bool
					for _, v := range template.Spec.Volumes {
						if r.Name == v.Name {
							foundRepo = true
							break
						}
					}
					assert.Assert(t, foundRepo)

					for _, c := range template.Spec.Containers {
						if c.Name == naming.ContainerDatabase ||
							c.Name == naming.PGBackRestRepoContainerName {
							var foundRepoVolMount bool
							for _, vm := range c.VolumeMounts {
								if vm.Name == r.Name {
									foundRepoVolMount = true
									break
								}
							}
							assert.Assert(t, foundRepoVolMount)
						}
					}
				}
			}

			// if a repo host is configured, then verify SSH is enabled
			if repoHost {

				// verify the ssh volume
				var foundSSHVolume bool
				var sshVolume v1.Volume
				for _, v := range template.Spec.Volumes {
					if v.Name == naming.PGBackRestSSHVolume {
						foundSSHVolume = true
						sshVolume = v
						break
					}
				}
				assert.Assert(t, foundSSHVolume)

				// verify the ssh config and secret
				var foundSSHConfigVolume, foundSSHSecretVolume bool
				defaultConfigName := naming.PGBackRestSSHConfig(postgresCluster).Name
				defaultSecretName := naming.PGBackRestSSHSecret(postgresCluster).Name
				for _, s := range sshVolume.Projected.Sources {
					if s.ConfigMap != nil {
						if (!customConfig && s.ConfigMap.Name == defaultConfigName) ||
							(customConfig && s.ConfigMap.Name == tc.sshConfig.Name) {
							foundSSHConfigVolume = true
						}
					} else if s.Secret != nil {
						if (!customSecret && s.Secret.Name == defaultSecretName) ||
							(customSecret && s.Secret.Name == tc.sshSecret.Name) {
							foundSSHSecretVolume = true
						}
					}
				}
				assert.Assert(t, foundSSHConfigVolume)
				assert.Assert(t, foundSSHSecretVolume)

				// verify that pgbackrest container is present and that the proper SSH volume mount in
				// present in all containers
				var foundSSHContainer bool
				for _, c := range template.Spec.Containers {
					if c.Name == naming.PGBackRestRepoContainerName {
						foundSSHContainer = true
					}
					var foundVolumeMount bool
					for _, vm := range c.VolumeMounts {
						if vm.Name == naming.PGBackRestSSHVolume && vm.MountPath == "/etc/ssh" &&
							vm.ReadOnly == true {
							foundVolumeMount = true
							break
						}
					}
					assert.Assert(t, foundVolumeMount)
				}
				assert.Assert(t, foundSSHContainer)
			}
		})
	}
}

func TestPodsToKeep(t *testing.T) {
	for _, test := range []struct {
		name      string
		instances []v1.Pod
		want      map[string]int
		checks    func(*testing.T, []v1.Pod)
	}{
		{
			name: "RemoveSetWithMasterOnly",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "RemoveSetWithReplicaOnly",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "KeepMasterOnly",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 1)
			},
		}, {
			name: "KeepNoRoleLabels",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 1)
			},
		}, {
			name: "RemoveSetWithNoRoleLabels",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "KeepUnknownRoleLabel",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "unknownLabelRole",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 1)
			},
		}, {
			name: "RemoveSetWithUnknownRoleLabel",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "unknownLabelRole",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 0)
			},
		}, {
			name: "MasterLastInSet",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 1,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 1)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "master")
			},
		}, {
			name: "ScaleDownSetWithMaster",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"max":   1,
				"daisy": 1,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 2)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "master")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "max")
			},
		}, {
			name: "ScaleDownSetWithoutMaster",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"max":   1,
				"daisy": 2,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 3)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "master")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "max")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[2].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[2].Labels[naming.LabelRole], "replica")
			},
		}, {
			name: "ScaleMasterSetToZero",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"max":   0,
				"daisy": 2,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 2)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "daisy")
			},
		}, {
			name: "RemoveMasterInstanceSet",
			instances: []v1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-asdf",
						Labels: map[string]string{
							naming.LabelRole:        "master",
							naming.LabelInstanceSet: "max",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-poih",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "daisy-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name: "max-dogs",
						Labels: map[string]string{
							naming.LabelRole:        "replica",
							naming.LabelInstanceSet: "daisy",
						},
					},
				},
			},
			want: map[string]int{
				"daisy": 3,
			},
			checks: func(t *testing.T, p []v1.Pod) {
				assert.Equal(t, len(p), 3)
				assert.Equal(t, p[0].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[0].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[1].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[1].Labels[naming.LabelInstanceSet], "daisy")
				assert.Equal(t, p[2].Labels[naming.LabelRole], "replica")
				assert.Equal(t, p[2].Labels[naming.LabelInstanceSet], "daisy")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			keep := podsToKeep(test.instances, test.want)
			sort.Slice(keep, func(i, j int) bool {
				return keep[i].Labels[naming.LabelRole] == "master"
			})
			test.checks(t, keep)
		})
	}
}

func TestDeleteInstance(t *testing.T) {
	env, cc, config := setupTestEnv(t, ControllerName)
	t.Cleanup(func() { teardownTestEnv(t, env) })

	reconciler := &Reconciler{}
	ctx, cancel := setupManager(t, config, func(mgr manager.Manager) {
		reconciler = &Reconciler{
			Client:   cc,
			Owner:    client.FieldOwner(t.Name()),
			Recorder: mgr.GetEventRecorderFor(ControllerName),
			Tracer:   otel.Tracer(t.Name()),
		}
	})
	t.Cleanup(func() { teardownManager(cancel, t) })

	ns := &v1.Namespace{}
	ns.GenerateName = "postgres-operator-test-"
	ns.Labels = map[string]string{"postgres-operator-test": t.Name()}
	assert.NilError(t, reconciler.Client.Create(ctx, ns))
	t.Cleanup(func() { assert.Check(t, reconciler.Client.Delete(ctx, ns)) })

	// Define, Create, and Reconcile a cluster to get an instance running in kube
	cluster := testCluster()
	cluster.Namespace = ns.Name

	assert.NilError(t, errors.WithStack(reconciler.Client.Create(ctx, cluster)))
	t.Cleanup(func() {
		// Remove finalizers, if any, so the namespace can terminate.
		assert.Check(t, client.IgnoreNotFound(
			reconciler.Client.Patch(ctx, cluster, client.RawPatch(
				client.Merge.Type(), []byte(`{"metadata":{"finalizers":[]}}`)))))
	})

	// Reconcile the entire cluster so that we don't have to create all the
	// resources needed to reconcile a single instance (cm,secrets,svc, etc.)
	result, err := reconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKeyFromObject(cluster),
	})
	assert.NilError(t, err)
	assert.Assert(t, result.Requeue == false)

	stsList := &appsv1.StatefulSetList{}
	assert.NilError(t, reconciler.Client.List(ctx, stsList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			naming.LabelCluster:     cluster.Name,
			naming.LabelInstanceSet: cluster.Spec.InstanceSets[0].Name,
		}))

	// Grab the instance name off of the instance set at index0
	instanceName := stsList.Items[0].Labels[naming.LabelInstance]

	// Use the instance name to delete the single instance
	assert.NilError(t, reconciler.deleteInstance(ctx, cluster, instanceName))

	gvks := []schema.GroupVersionKind{
		corev1.SchemeGroupVersion.WithKind("PersistentVolumeClaim"),
		corev1.SchemeGroupVersion.WithKind("ConfigMap"),
		corev1.SchemeGroupVersion.WithKind("Secret"),
		appsv1.SchemeGroupVersion.WithKind("StatefulSet"),
	}

	selector, err := naming.AsSelector(metav1.LabelSelector{
		MatchLabels: map[string]string{
			naming.LabelCluster:  cluster.Name,
			naming.LabelInstance: instanceName,
		}})
	assert.NilError(t, err)

	for _, gvk := range gvks {
		t.Run(gvk.Kind, func(t *testing.T) {
			uList := &unstructured.UnstructuredList{}
			err := wait.Poll(time.Second*3, time.Second*30, func() (done bool, err error) {
				uList.SetGroupVersionKind(gvk)
				assert.NilError(t, errors.WithStack(reconciler.Client.List(ctx, uList,
					client.InNamespace(cluster.Namespace),
					client.MatchingLabelsSelector{Selector: selector})))

				if len(uList.Items) == 0 {
					return true, nil
				}

				// Check existing objects for deletionTimestamp ensuring they
				// are staged for delete
				deleted := true
				for i := range uList.Items {
					u := uList.Items[i]
					if u.GetDeletionTimestamp() == nil {
						deleted = false
					}
				}

				// We have found objects that are not staged for delete
				// so deleteInstance has failed
				return deleted, nil
			})
			assert.NilError(t, err)
		})
	}
}

func TestGenerateInstanceStatefulSetIntent(t *testing.T) {
	type intentParams struct {
		cluster                    *v1beta1.PostgresCluster
		spec                       *v1beta1.PostgresInstanceSetSpec
		clusterPodServiceName      string
		instanceServiceAccountName string
		sts                        *appsv1.StatefulSet
		shutdown                   bool
		startupInstance            string
		numInstancePods            int
	}

	for _, test := range []struct {
		name string
		ip   intentParams
		run  func(*testing.T, *appsv1.StatefulSet)
	}{{
		name: "cluster pod service name",
		ip: intentParams{
			clusterPodServiceName: "daisy-svc",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, ss.Spec.ServiceName, "daisy-svc")
		},
	}, {
		name: "instance service account name",
		ip: intentParams{
			instanceServiceAccountName: "daisy-sa",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, ss.Spec.Template.Spec.ServiceAccountName, "daisy-sa")
		},
	}, {
		name: "custom affinity",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				Affinity: &corev1.Affinity{},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Assert(t, ss.Spec.Template.Spec.Affinity != nil)
		},
	}, {
		name: "custom tolerations",
		ip: intentParams{
			spec: &v1beta1.PostgresInstanceSetSpec{
				Tolerations: []v1.Toleration{},
			},
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Assert(t, ss.Spec.Template.Spec.Tolerations != nil)
		},
	}, {
		name: "shutdown replica",
		ip: intentParams{
			shutdown:        true,
			numInstancePods: 2,
			startupInstance: "testInstance1",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(0))
		},
	}, {
		name: "shutdown primary",
		ip: intentParams{
			shutdown:        true,
			numInstancePods: 1,
			startupInstance: "testInstance1",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(0))
		},
	}, {
		name: "startup primary",
		ip: intentParams{
			shutdown:        false,
			numInstancePods: 0,
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(1))
		},
	}, {
		name: "startup replica",
		ip: intentParams{
			shutdown:        false,
			numInstancePods: 1,
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(1))
		},
	}, {
		name: "do not startup replica",
		ip: intentParams{
			shutdown:        false,
			numInstancePods: 0,
			startupInstance: "testInstance1",
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(0))
		},
	}, {
		name: "do not shutdown primary",
		ip: intentParams{
			shutdown:        true,
			numInstancePods: 2,
		},
		run: func(t *testing.T, ss *appsv1.StatefulSet) {
			assert.Equal(t, *ss.Spec.Replicas, int32(1))
		},
	}} {
		t.Run(test.name, func(t *testing.T) {

			cluster := test.ip.cluster
			if cluster == nil {
				cluster = testCluster()
			}

			cluster.Default()
			cluster.UID = types.UID("hippouid")
			cluster.Namespace = test.name + "-ns"
			cluster.Spec.Shutdown = &test.ip.shutdown
			cluster.Status.StartupInstance = test.ip.startupInstance

			spec := test.ip.spec
			if spec == nil {
				spec = new(v1beta1.PostgresInstanceSetSpec)
				spec.Default(0)
			}

			clusterPodServiceName := test.ip.clusterPodServiceName
			instanceServiceAccountName := test.ip.instanceServiceAccountName
			sts := test.ip.sts
			if sts == nil {
				sts = &appsv1.StatefulSet{}
			}

			generateInstanceStatefulSetIntent(context.Background(),
				cluster, spec,
				clusterPodServiceName,
				instanceServiceAccountName,
				sts,
				test.ip.numInstancePods,
			)

			test.run(t, sts)
		})
	}
}

func TestFindAvailableInstanceNames(t *testing.T) {

	testCases := []struct {
		set                   v1beta1.PostgresInstanceSetSpec
		fakeObservedInstances *observedInstances
		fakeClusterVolumes    []v1.PersistentVolumeClaim
		expectedInstanceNames []string
	}{{
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{}},
			}},
			[]appsv1.StatefulSet{{}},
			[]v1.Pod{},
		),
		fakeClusterVolumes:    []v1.PersistentVolumeClaim{{}},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]v1.Pod{},
		),
		fakeClusterVolumes: []v1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "instance1-abc-def",
			Labels: map[string]string{
				naming.LabelRole:        naming.RolePostgresData,
				naming.LabelInstanceSet: "instance1",
				naming.LabelInstance:    "instance1-abc"}}}},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]v1.Pod{},
		),
		fakeClusterVolumes:    []v1.PersistentVolumeClaim{},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]v1.Pod{},
		),
		fakeClusterVolumes: []v1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-def",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-abc"}}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-efg",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-def"}}},
		},
		expectedInstanceNames: []string{"instance1-def"},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1"},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]v1.Pod{},
		),
		fakeClusterVolumes: []v1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "instance1-abc-def",
			Labels: map[string]string{
				naming.LabelRole:        naming.RolePostgresData,
				naming.LabelInstanceSet: "instance1",
				naming.LabelInstance:    "instance1-def"}}}},
		expectedInstanceNames: []string{"instance1-def"},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1",
			WALVolumeClaimSpec: &v1.PersistentVolumeClaimSpec{}},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc",
				Labels: map[string]string{
					naming.LabelInstanceSet: "instance1"}}}},
			[]v1.Pod{},
		),
		fakeClusterVolumes: []v1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-def",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-abc"}}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-abc-def",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresWAL,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-abc"}}}},
		expectedInstanceNames: []string{},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1",
			WALVolumeClaimSpec: &v1.PersistentVolumeClaimSpec{}},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{},
			[]v1.Pod{},
		),
		fakeClusterVolumes: []v1.PersistentVolumeClaim{
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-def-ghi",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresData,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-def"}}},
			{ObjectMeta: metav1.ObjectMeta{
				Name: "instance1-def-jkl",
				Labels: map[string]string{
					naming.LabelRole:        naming.RolePostgresWAL,
					naming.LabelInstanceSet: "instance1",
					naming.LabelInstance:    "instance1-def"}}}},
		expectedInstanceNames: []string{"instance1-def"},
	}, {
		set: v1beta1.PostgresInstanceSetSpec{Name: "instance1",
			WALVolumeClaimSpec: &v1.PersistentVolumeClaimSpec{}},
		fakeObservedInstances: newObservedInstances(
			&v1beta1.PostgresCluster{Spec: v1beta1.PostgresClusterSpec{
				InstanceSets: []v1beta1.PostgresInstanceSetSpec{{Name: "instance1"}},
			}},
			[]appsv1.StatefulSet{},
			[]v1.Pod{},
		),
		fakeClusterVolumes: []v1.PersistentVolumeClaim{{ObjectMeta: metav1.ObjectMeta{
			Name: "instance1-def-ghi",
			Labels: map[string]string{
				naming.LabelRole:        naming.RolePostgresData,
				naming.LabelInstanceSet: "instance1",
				naming.LabelInstance:    "instance1-def"}}}},
		expectedInstanceNames: []string{},
	}}

	for _, tc := range testCases {
		var walEnabled string
		if tc.set.WALVolumeClaimSpec != nil {
			walEnabled = ", WAL volume enabled"
		}
		name := fmt.Sprintf("%d set(s), %d volume(s)%s: expect %d instance names(s)",
			len(tc.fakeObservedInstances.setNames), len(tc.fakeClusterVolumes), walEnabled,
			len(tc.expectedInstanceNames))
		t.Run(name, func(t *testing.T) {
			assert.DeepEqual(t, findAvailableInstanceNames(tc.set, tc.fakeObservedInstances,
				tc.fakeClusterVolumes), tc.expectedInstanceNames)
		})
	}
}
