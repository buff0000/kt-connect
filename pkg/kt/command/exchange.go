package command

import (
	"context"
	"errors"
	"fmt"
	"github.com/alibaba/kt-connect/pkg/common"
	"github.com/alibaba/kt-connect/pkg/kt"
	"github.com/alibaba/kt-connect/pkg/kt/cluster"
	"github.com/alibaba/kt-connect/pkg/kt/connect"
	"github.com/alibaba/kt-connect/pkg/kt/options"
	"github.com/alibaba/kt-connect/pkg/kt/util"
	"github.com/alibaba/kt-connect/pkg/process"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	urfave "github.com/urfave/cli"
	appV1 "k8s.io/api/apps/v1"
	coreV1 "k8s.io/api/core/v1"
	"os"
	"strings"
	"time"
)

// newExchangeCommand return new exchange command
func newExchangeCommand(cli kt.CliInterface, options *options.DaemonOptions, action ActionInterface) urfave.Command {
	return urfave.Command{
		Name:  "exchange",
		Usage: "exchange kubernetes deployment to local",
		Flags: []urfave.Flag{
			urfave.StringFlag{
				Name:        "expose",
				Usage:       "ports to expose separate by comma, in [port] or [local:remote] format, e.g. 7001,8080:80",
				Destination: &options.ExchangeOptions.Expose,
			},
			urfave.StringFlag{
				Name:        "method",
				Value:       "scale",
				Usage:       "Exchange method 'scale' or 'ephemeral'(beta)",
				Destination: &options.ExchangeOptions.Method,
			},
		},
		Action: func(c *urfave.Context) error {
			if options.Debug {
				zerolog.SetGlobalLevel(zerolog.DebugLevel)
			}
			if err := combineKubeOpts(options); err != nil {
				return err
			}
			resourceToExchange := c.Args().First()
			expose := options.ExchangeOptions.Expose

			if len(resourceToExchange) == 0 {
				return errors.New("name of resource to exchange is required")
			}
			if len(expose) == 0 {
				return errors.New("--expose is required")
			}

			return action.Exchange(resourceToExchange, cli, options)
		},
	}
}

//Exchange exchange kubernetes workload
func (action *Action) Exchange(resourceName string, cli kt.CliInterface, options *options.DaemonOptions) error {
	ch, err := setupProcess(cli, options, common.ComponentExchange)
	if err != nil {
		return err
	}

	method := options.ExchangeOptions.Method
	if method == common.ExchangeMethodScale {
		err = exchangeByScale(resourceName, cli, options)
	} else if method == common.ExchangeMethodEphemeral {
		err = exchangeByEphemeralContainer(resourceName, cli, options)
	} else {
		err = fmt.Errorf("invalid exchange method \"%s\"", method)
	}
	if err != nil {
		return err
	}

	// watch background process, clean the workspace and exit if background process occur exception
	go func() {
		<-process.Interrupt()
		log.Error().Msgf("Command interrupted: %s", <-process.Interrupt())
		CleanupWorkspace(cli, options)
		os.Exit(0)
	}()
	s := <-ch
	log.Info().Msgf("Terminal Signal is %s", s)
	return nil
}

func exchangeByScale(deploymentName string, cli kt.CliInterface, options *options.DaemonOptions) error {
	kubernetes, err := cli.Kubernetes()
	if err != nil {
		return err
	}
	ctx := context.Background()
	app, err := kubernetes.Deployment(ctx, deploymentName, options.Namespace)
	if err != nil {
		return err
	}

	// record context inorder to remove after command exit
	options.RuntimeOptions.Origin = app.GetName()
	options.RuntimeOptions.Replicas = *app.Spec.Replicas

	shadowPodName := app.GetName() + "-kt-" + strings.ToLower(util.RandomString(5))

	envs := make(map[string]string)
	podIP, podName, sshConfigMapName, credential, err := kubernetes.GetOrCreateShadow(ctx, shadowPodName, options,
		getExchangeLabels(options, shadowPodName, app), getExchangeAnnotation(options), envs)
	log.Info().Msgf("Create exchange shadow %s in namespace %s", shadowPodName, options.Namespace)

	if err != nil {
		return err
	}

	// record data
	options.RuntimeOptions.Shadow = shadowPodName
	options.RuntimeOptions.SSHCM = sshConfigMapName

	down := int32(0)
	if err = kubernetes.Scale(ctx, app, &down); err != nil {
		return err
	}

	shadow := connect.Create(options)
	if err = shadow.Inbound(options.ExchangeOptions.Expose, podName, podIP, credential); err != nil {
		return err
	}

	return nil
}

func exchangeByEphemeralContainer(resourceName string, cli kt.CliInterface, options *options.DaemonOptions) error {
	log.Warn().Msgf("Experimental feature. It just works on kubernetes above v1.23. It can NOT work with istio now.")
	k8s, err := cli.Kubernetes()
	if err != nil {
		return err
	}

	ctx := context.Background()
	pods, err := getPodsOfResource(ctx, k8s, resourceName, options.Namespace)
	containerName := "kt-" + strings.ToLower(util.RandomString(5))

	for _, pod := range pods {
		if pod.Status.Phase != coreV1.PodRunning {
			log.Warn().Msgf("Pod %s is not running (%s), will not be exchanged", pod.Name, pod.Status.Phase)
			continue
		}
		log.Info().Msgf("Adding ephemeral container for pod %s", pod.Name)

		envs := make(map[string]string)
		sshConfigMapName, err := k8s.AddEphemeralContainer(ctx, containerName, pod.Name, options, envs)
		if err != nil {
			return err
		}

	breakLoop:
		for i := 0; i < 100; i++ {
			log.Info().Msgf("Waiting for ephemeral container %s to be ready", containerName)
			pod, err := k8s.Pod(ctx, pod.Name, options.Namespace)
			if err != nil {
				return err
			}
			cStats := pod.Status.EphemeralContainerStatuses
			for i := range cStats {
				if cStats[i].Name == containerName {
					if cStats[i].State.Running != nil {
						break breakLoop
					} else if cStats[i].State.Terminated != nil {
						log.Error().Msgf("Ephemeral container %s is terminated, code: %d",
							containerName, cStats[i].State.Terminated.ExitCode)
					}
				}
			}
			time.Sleep(2 * time.Second)
		}

		// record data
		options.RuntimeOptions.PodName = pod.Name
		options.RuntimeOptions.SSHCM = sshConfigMapName

		shadow := connect.Create(options)
		if err = shadow.Inbound(options.ExchangeOptions.Expose, pod.Name, pod.Status.PodIP, nil); err != nil {
			return err
		}
	}
	return nil
}

func getPodsOfResource(ctx context.Context, k8s cluster.KubernetesInterface, resourceName, namespace string) ([]coreV1.Pod, error) {
	segments := strings.Split(resourceName, "/")
	var resourceType, name string
	if len(segments) > 2 {
		return nil, fmt.Errorf("invalid resource name: %s", resourceName)
	} else if len(segments) == 2 {
		resourceType = segments[0]
		name = segments[1]
	} else {
		resourceType = "pod"
		name = resourceName
	}

	switch resourceType {
	case "pod":
		pod, err := k8s.Pod(ctx, name, namespace)
		if err != nil {
			return nil, err
		} else {
			return []coreV1.Pod{*pod}, nil
		}
	case "service":
	case "svc":
		return getPodsOfService(ctx, k8s, name, namespace)
	}
	return nil, fmt.Errorf("invalid resource type: %s", resourceType)
}

func getPodsOfService(ctx context.Context, k8s cluster.KubernetesInterface, serviceName, namespace string) ([]coreV1.Pod, error) {
	svc, err := k8s.Service(ctx, serviceName, namespace)
	if err != nil {
		return nil, err
	}
	pods, err := k8s.Pods(ctx, svc.Spec.Selector, namespace)
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}

func getExchangeAnnotation(options *options.DaemonOptions) map[string]string {
	return map[string]string{
		common.KTConfig: fmt.Sprintf("app=%s,replicas=%d",
			options.RuntimeOptions.Origin, options.RuntimeOptions.Replicas),
	}
}

func getExchangeLabels(options *options.DaemonOptions, workload string, origin *appV1.Deployment) map[string]string {
	labels := map[string]string{
		common.ControlBy:   common.KubernetesTool,
		common.KTComponent: common.ComponentExchange,
		common.KTName:      workload,
	}
	if origin != nil {
		for k, v := range origin.Spec.Selector.MatchLabels {
			labels[k] = v
		}
	}
	splits := strings.Split(workload, "-")
	labels[common.KTVersion] = splits[len(splits)-1]
	return labels
}
