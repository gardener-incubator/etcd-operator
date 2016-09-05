package cluster

import (
	"fmt"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/kube-etcd-controller/pkg/util/etcdutil"
	"golang.org/x/net/context"
)

// reconcile reconciles
// - the members in the cluster view with running pods in Kubernetes.
// - the members and expect size of cluster.
//
// Definitions:
// - running pods in k8s cluster
// - members in controller knowledge
// Steps:
// 1. Remove all pods from running set that does not belong to member set.
// 2. L consist of remaining pods of runnings
// 3. If L = members, the current state matches the membership state. END.
// 4. If len(L) < len(members)/2 + 1, quorum lost. Go to recovery process (TODO).
// 5. Add one missing member. END.
func (c *Cluster) reconcile(running etcdutil.MemberSet) error {
	log.Println("Reconciling:")
	defer func() {
		log.Println("Finish Reconciling")
	}()

	if len(c.members) == 0 {
		cfg := clientv3.Config{
			Endpoints:   running.ClientURLs(),
			DialTimeout: 5 * time.Second,
		}
		etcdcli, err := clientv3.New(cfg)
		if err != nil {
			return err
		}
		if err := etcdutil.WaitMemberReady(etcdcli); err != nil {
			return err
		}
		c.updateMembers(etcdcli)
	}

	log.Println("Running pods:", running)
	log.Println("Expected membership:", c.members)

	unknownMembers := running.Diff(c.members)
	if unknownMembers.Size() > 0 {
		log.Println("Removing unexpected pods:", unknownMembers)
		for _, m := range unknownMembers {
			if err := c.removePodAndService(m.Name); err != nil {
				return err
			}
		}
	}
	L := running.Diff(unknownMembers)

	if L.Size() == c.members.Size() {
		return c.resize()
	}

	if L.Size() < c.members.Size()/2+1 {
		log.Println("Disaster recovery")
		return c.disasterRecovery(L)
	}

	log.Println("Recovering one member")
	toRecover := c.members.Diff(L).PickOne()

	if err := c.removeMember(toRecover); err != nil {
		return err
	}
	return c.resize()
}

func (c *Cluster) resize() error {
	if c.members.Size() == c.spec.Size {
		return nil
	}

	if c.members.Size() < c.spec.Size {
		return c.addOneMember()
	}

	return c.removeOneMember()
}

func (c *Cluster) addOneMember() error {
	cfg := clientv3.Config{
		Endpoints:   c.members.ClientURLs(),
		DialTimeout: 5 * time.Second,
	}
	etcdcli, err := clientv3.New(cfg)
	if err != nil {
		return err
	}
	newMemberName := fmt.Sprintf("%s-%04d", c.name, c.idCounter)
	newMember := &etcdutil.Member{Name: newMemberName}
	resp, err := etcdcli.MemberAdd(context.TODO(), []string{newMember.PeerAddr()})
	if err != nil {
		return err
	}
	newMember.ID = resp.Member.ID
	c.members.Add(newMember)

	if err := c.createPodAndService(c.members, newMember, "existing"); err != nil {
		return err
	}
	c.idCounter++
	log.Printf("added member, cluster: %s", c.members.PeerURLPairs())
	return nil
}

func (c *Cluster) removeOneMember() error {
	return c.removeMember(c.members.PickOne())
}

func (c *Cluster) removeMember(toRemove *etcdutil.Member) error {
	cfg := clientv3.Config{
		Endpoints:   c.members.ClientURLs(),
		DialTimeout: 5 * time.Second,
	}
	etcdcli, err := clientv3.New(cfg)
	if err != nil {
		return err
	}

	clustercli := clientv3.NewCluster(etcdcli)
	if _, err := clustercli.MemberRemove(context.TODO(), toRemove.ID); err != nil {
		return err
	}
	c.members.Remove(toRemove.Name)
	if err := c.removePodAndService(toRemove.Name); err != nil {
		return err
	}
	log.Printf("removed member (%v) with ID (%d)", toRemove.Name, toRemove.ID)
	return nil
}

func (c *Cluster) disasterRecovery(left etcdutil.MemberSet) error {
	httpClient := c.kclient.RESTClient.Client
	name := fmt.Sprintf("%s-backup-tool", c.name)
	_, err := httpClient.Get(fmt.Sprintf("http://%s/backupnow", name))
	if err != nil {
		log.Error(err)
		return err
	}
	log.Info("Made a latest backup successfully")

	for _, m := range left {
		err := c.removePodAndService(m.Name)
		if err != nil {
			return err
		}
	}
	c.members = nil
	// TODO: config the seed member to get backup first
	return c.startSeedMember(c.spec)
}