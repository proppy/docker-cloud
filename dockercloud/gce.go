//
// Copyright (C) 2013 The Docker Cloud authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package dockercloud

import (
	"code.google.com/p/goauth2/oauth"
	compute "code.google.com/p/google-api-go-client/compute/v1"
	"net/http"
	"path"

	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

var (
	projectId             = flag.String("project", "", "Google Cloud Project Name")
	gcloudCredentialsPath = flag.String("gcloudcredentials", path.Join(os.Getenv("HOME"), ".config/gcloud/credentials"), "gcloud SDK credentials path")
	instanceType          = flag.String("instancetype",
		"/zones/us-central1-a/machineTypes/n1-standard-1",
		"The reference to the instance type to create.")
	image = flag.String("image",
		"https://www.googleapis.com/compute/v1/projects/debian-cloud/global/images/backports-debian-7-wheezy-v20131127",
		"The GCE image to boot from.")
	diskName   = flag.String("diskname", "docker-root", "Name of the instance root disk")
	diskSizeGb = flag.Int64("disksize", 100, "Size of the root disk in GB")
)

const startup = `#!/bin/bash
sysctl -w net.ipv4.ip_forward=1
wget -qO- https://get.docker.io/ | sh
echo 'DOCKER_OPTS="-H :8000 -mtu 1460"' >> /etc/default/docker
service docker restart && echo "docker restarted on port :8000"
`

// A Google Compute Engine implementation of the Cloud interface
type GCECloud struct {
	service   *compute.Service
	projectId string
}

type gcloudCredentialsCache struct {
	Data []struct {
		Credential struct {
			Client_Id     string
			Client_Secret string
			Access_Token  string
			Refresh_Token string
			Token_Expiry  time.Time
		}
		Key struct {
			Scope string
		}
	}
}

func gcloudTransport() (*oauth.Transport, error) {
	f, err := os.Open(*gcloudCredentialsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	cache := &gcloudCredentialsCache{}
	if err := json.NewDecoder(f).Decode(cache); err != nil {
		return nil, err
	}
	log.Print(cache)
	gcloud := cache.Data[0]
	t := &oauth.Transport{
		Config: &oauth.Config{
			ClientId:     gcloud.Credential.Client_Id,
			ClientSecret: gcloud.Credential.Client_Secret,
			RedirectURL:  "oob",
			Scope:        gcloud.Key.Scope,
			AuthURL:      "https://accounts.google.com/o/oauth2/auth",
			TokenURL:     "https://accounts.google.com/o/oauth2/token",
		},
		Token: &oauth.Token{
			AccessToken:  gcloud.Credential.Access_Token,
			RefreshToken: gcloud.Credential.Refresh_Token,
			Expiry:       gcloud.Credential.Token_Expiry,
		},
		Transport: http.DefaultTransport,
	}
	return t, t.Refresh()
}

// Create a GCE Cloud instance.
func NewGCECloud() Cloud {
	// Set up a gcloud transport.
	transport, err := gcloudTransport()
	if err != nil {
		log.Fatalf("unable to create gcloud transport: %v", err)
	}

	svc, err := compute.New(transport.Client())
	if err != nil {
		log.Fatalf("Error creating service: %v", err)
	}
	return &GCECloud{
		service:   svc,
		projectId: *projectId,
	}
}

// Implementation of the Cloud interface
func (cloud GCECloud) GetPublicIPAddress(name string, zone string) (string, error) {
	instance, err := cloud.service.Instances.Get(cloud.projectId, zone, name).Do()
	if err != nil {
		return "", err
	}
	// Found the instance, we're good.
	return instance.NetworkInterfaces[0].AccessConfigs[0].NatIP, nil
}

// Get or create a new root disk.
func (cloud GCECloud) getOrCreateRootDisk(name, zone string) (string, error) {
	log.Printf("try getting root disk: %q", name)
	disk, err := cloud.service.Disks.Get(cloud.projectId, zone, *diskName).Do()
	if err == nil {
		log.Printf("found %q", disk.SelfLink)
		return disk.SelfLink, nil
	}
	log.Printf("not found, creating root disk: %q", name)
	op, err := cloud.service.Disks.Insert(cloud.projectId, zone, &compute.Disk{
		Name: *diskName,
	}).SourceImage(*image).Do()
	if err != nil {
		log.Printf("disk insert api call failed: %v", err)
		return "", err
	}
	err = cloud.waitForOp(op, zone)
	if err != nil {
		log.Printf("disk insert operation failed: %v", err)
		return "", err
	}
	log.Printf("root disk created: %q", op.TargetLink)
	return op.TargetLink, nil
}

// Implementation of the Cloud interface
func (cloud GCECloud) CreateInstance(name string, zone string) (string, error) {
	rootDisk, err := cloud.getOrCreateRootDisk(*diskName, zone)
	if err != nil {
		log.Printf("failed to create root disk: %v", err)
		return "", err
	}
	prefix := "https://www.googleapis.com/compute/v1/projects/" + cloud.projectId
	instance := &compute.Instance{
		Name:        name,
		Description: "Docker on GCE",
		MachineType: prefix + *instanceType,
		Disks: []*compute.AttachedDisk{
			{
				Boot:   true,
				Type:   "PERSISTENT",
				Mode:   "READ_WRITE",
				Source: rootDisk,
			},
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				AccessConfigs: []*compute.AccessConfig{
					&compute.AccessConfig{Type: "ONE_TO_ONE_NAT"},
				},
				Network: prefix + "/global/networks/default",
			},
		},
		Metadata: &compute.Metadata{
			Items: []*compute.MetadataItems{
				{
					Key:   "startup-script",
					Value: startup,
				},
			},
		},
	}
	log.Printf("starting instance: %q", name)
	op, err := cloud.service.Instances.Insert(cloud.projectId, zone, instance).Do()
	if err != nil {
		log.Printf("instance insert api call failed: %v", err)
		return "", err
	}
	err = cloud.waitForOp(op, zone)
	if err != nil {
		log.Printf("instance insert operation failed: %v", err)
		return "", err
	}

	// Wait for docker to come up
	// TODO(bburns) : Use metadata instead to signal that docker is up and read.
	time.Sleep(60 * time.Second)

	log.Printf("instance started: %q", instance.NetworkInterfaces[0].AccessConfigs[0].NatIP)
	return instance.NetworkInterfaces[0].AccessConfigs[0].NatIP, err
}

// Implementation of the Cloud interface
func (cloud GCECloud) DeleteInstance(name string, zone string) error {
	log.Print("deleting instance")
	op, err := cloud.service.Instances.Delete(cloud.projectId, zone, name).Do()
	if err != nil {
		log.Printf("Got compute.Operation, err: %#v, %v", op, err)
		return err
	}
	err = cloud.waitForOp(op, zone)
	log.Print("instance deleted")
	return err
}

func (cloud GCECloud) OpenSecureTunnel(name, zone string, localPort, remotePort int) (*os.Process, error) {
	return cloud.openSecureTunnel(name, zone, "localhost", localPort, remotePort)
}

func (cloud GCECloud) openSecureTunnel(name, zone, hostname string, localPort, remotePort int) (*os.Process, error) {
	ip, err := cloud.GetPublicIPAddress(name, zone)
	if err != nil {
		return nil, err
	}
	username := os.Getenv("USER")
	homedir := os.Getenv("HOME")

	sshCommand := fmt.Sprintf("-o LogLevel=quiet -o UserKnownHostsFile=/dev/null -o CheckHostIP=no -o StrictHostKeyChecking=no -i %s/.ssh/google_compute_engine -A -p 22 %s@%s -f -N -L %d:%s:%d", homedir, username, ip, localPort, hostname, remotePort)
	log.Printf("Running %s", sshCommand)
	cmd := exec.Command("ssh", strings.Split(sshCommand, " ")...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	cmd.Run()
	return cmd.Process, nil
}

// Wait for a compute operation to finish.
//   op The operation
//   zone The zone for the operation
// Returns an error if one occurs, or nil
func (cloud GCECloud) waitForOp(op *compute.Operation, zone string) error {
	op, err := cloud.service.ZoneOperations.Get(cloud.projectId, zone, op.Name).Do()
	for op.Status != "DONE" {
		fmt.Print(".")
		time.Sleep(5 * time.Second)
		op, err = cloud.service.ZoneOperations.Get(cloud.projectId, zone, op.Name).Do()
		if err != nil {
			log.Printf("Got compute.Operation, err: %#v, %v", op, err)
		}
		if op.Status != "PENDING" && op.Status != "RUNNING" && op.Status != "DONE" {
			log.Printf("Error waiting for operation: %s\n", op)
			return errors.New(fmt.Sprintf("Bad operation: %s", op))
		}
	}
	fmt.Print("\n")
	return err
}
