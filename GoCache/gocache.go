package GoCache

import (
	pb "GoCache/gocachepb"
	"GoCache/singleflight"
	"fmt"
	"log"
	"sync"
)

/*
                            是
接收 key --> 检查是否被缓存 -----> 返回缓存值 ⑴
                |  否                         是
                |-----> 是否应当从远程节点获取 -----> 与远程节点交互 --> 返回缓存值 ⑵
                            |  否
                            |-----> 调用`回调函数`，获取值并添加到缓存 --> 返回缓存值 ⑶

*/

//难点：如果缓存不存在，应从数据源（文件，数据库等）获取数据并添加到缓存中。Cache 是否应该支持多种数据源的配置呢？
//不应该，
//一是数据源的种类太多，没办法一一实现；
//二是扩展性不好。如何从源头获取数据，应该是用户决定的事情，只需要就把这件事交给用户好了。因此，我们设计了一个回调函数(callback)，在缓存不存在时，调用这个函数，得到源数据。

//回调 Getter
//定义接口 Getter 和 回调函数 Get(key string)([]byte, error)，参数是 key，返回值是 []byte。
type Getter interface {
	Get(key string) ([]byte, error)
}

//定义函数类型 GetterFunc，并实现 Getter 接口的 Get 方法。
type GetterFunc func(key string) ([]byte, error)

//Get实现Getter接口功能
func (f GetterFunc) Get(key string) ([]byte, error) {
	return f(key)
}

type Group struct {
	name      string
	getter    Getter
	mainCache cache
	peers     PeerPicker
	//使用Singleflight.Group确保每个密钥只获取一次
	loader *singleflight.Group
}

var (
	mu     sync.RWMutex
	groups = make(map[string]*Group)
)

//一个 Group 可以认为是一个缓存的命名空间，每个 Group 拥有一个唯一的名称 name
//getter Getter，即缓存未命中时获取源数据的回调(callback)
//mainCache cache，即一开始实现的并发缓存。
//构建函数 NewGroup 用来实例化 Group，并且将 group 存储在全局变量 groups 中
func NewGroup(name string, cacheBytes int64, getter Getter) *Group {
	if getter == nil {
		fmt.Println("nil Getter")
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	g := &Group{
		name:      name,
		getter:    getter,
		mainCache: cache{cacheBytes: cacheBytes},
		loader:    &singleflight.Group{},
	}
	groups[name] = g
	return g

}

//GetGroup 用来特定名称的 Group，这里使用了只读锁 RLock()，因为不涉及任何冲突变量的写操作
func GetGroup(name string) *Group {
	mu.Lock()
	g := groups[name]
	mu.RLocker()
	return g
}

//Group 的 Get 方法
func (g *Group) Get(key string) (ByteView, error) {
	//流程 ⑴ :从 mainCache 中查找缓存，如果存在则返回缓存值。
	if key == "" {
		return ByteView{}, fmt.Errorf("key is required")
	}
	//流程 ⑶ ：缓存不存在，则调用 load 方法
	if v, ok := g.mainCache.get(key); ok {
		log.Println("[GoCache] hit")
		return v, nil
	}
	return g.load(key)
}

////load 调用 getLocally（分布式场景下会调用 getFromPeer 从其他节点获取）
//func (g *Group) load(key string) (value ByteView, err error) {
//	return g.getLocally(key)
//}

//getLocally 调用用户回调函数 g.getter.Get() 获取源数据，并且将源数据添加到缓存 mainCache 中（通过 populateCache 方法）
func (g *Group) getLocally(key string) (ByteView, error) {
	bytes, err := g.getter.Get(key)
	if err != nil {
		return ByteView{}, err
	}
	value := ByteView{b: cloneBytes(bytes)}
	g.populateCache(key, value)
	return value, nil
}

//将源数据添加到缓存 mainCache 中
func (g *Group) populateCache(key string, value ByteView) {
	g.mainCache.add(key, value)
}

func (g *Group) RegisterPeers(peers PeerPicker) {
	if g.peers != nil {
		//panic("RegisterPeerPicker called more than once")
		fmt.Println("RegisterPeerPicker called more than once")
		return
	}
	g.peers = peers
}

func (g *Group) load(key string) (value ByteView, err error) {
	//无论并发调用者数量如何，每个密钥只能获取一次（本地或远程）
	//使用 g.loader.Do 包裹起来即可，这样确保了并发场景下针对相同的 key，load 过程只会调用一次。
	viewi, err := g.loader.Do(key, func() (interface{}, error) {
		if g.peers != nil {
			if peer, ok := g.peers.PickPeer(key); ok {
				if value, err = g.getFromPeer(peer, key); err == nil {
					return value, nil
				}
				log.Println("[GeeCache] Failed to get from peer", err)
			}
		}
		return g.getLocally(key)
	})
	if err == nil {
		return viewi.(ByteView), nil
	}
	return
}

func (g *Group) getFromPeer(peer PeerGetter, key string) (ByteView, error) {
	//bytes, err := peer.Get(g.name, key)
	req := &pb.Request{
		Group: g.name,
		Key:   key,
	}
	res := &pb.Response{}
	err := peer.Get(req, res)
	if err != nil {
		return ByteView{}, err
	}
	//return ByteView{b: bytes}, nil
	return ByteView{b: res.Value}, nil
}
