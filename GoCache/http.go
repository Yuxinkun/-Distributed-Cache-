package GoCache

import (
	"GoCache/consistenthash"
	pb "GoCache/gocachepb"
	"fmt"
	"google.golang.org/protobuf/proto"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

//为 HTTPPool 添加节点选择的功能
const (
	defultBasePath  = "/_gocache/"
	defaultReplicas = 50
)

//HTTPPool 只有 2 个参数，
//一个是 self，用来记录自己的地址，包括主机名/IP 和端口。
//另一个是 basePath，作为节点间通讯地址的前缀，默认是 /_gocache/，
//那么 http://example.com/_gocache/ 开头的请求，就用于节点间的访问。
//因为一个主机上还可能承载其他的服务，加一段 Path 是一个好习惯。比如，大部分网站的 API 接口，一般以 /api 作为前缀。

type HTTPPool struct {
	self     string
	basePath string
	mu       sync.Mutex
	//新增成员变量 peers，类型是一致性哈希算法的 Map，用来根据具体的 key 选择节点
	peers *consistenthash.Map
	//新增成员变量 httpGetters，映射远程节点与对应的 httpGetter。
	//每一个远程节点对应一个 httpGetter，因为 httpGetter 与远程节点的地址 baseURL 有关。
	httpGetters map[string]*httpGetter
}

//baseURL 表示将要访问的远程节点的地址，例如 http://example.com/_gocache/
type httpGetter struct {
	baseURL string
}

//NewHTTPPool初始化对等方的HTTP池
func NewHTTPPool(self string) *HTTPPool {
	defaultBasePath := defultBasePath
	return &HTTPPool{
		self:     self,
		basePath: defaultBasePath,
	}
}

/*
分布式缓存需要实现节点间通信，建立基于 HTTP 的通信机制是比较常见和简单的做法。
如果一个节点启动了 HTTP 服务，那么这个节点就可以被其他节点访问。
http.go这节就是为单机节点搭建 HTTP Server。
*/
//使用服务器名称记录信息
func (p *HTTPPool) Log(format string, v ...interface{}) {
	log.Printf("[Server %s] %s", p.self, fmt.Sprintf(format, v))
}

//ServeHTTP处理所有http请求
func (p *HTTPPool) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, p.basePath) {
		str := "HTTPPool serving unexpected path: " + r.URL.Path
		fmt.Println("error:" + str)
		return
	}
	p.Log("%s %s", r.Method, r.URL.Path)

	// /<basepath>/<groupname>/<key> 必填
	parts := strings.SplitN(string(r.URL.Path[len(p.basePath)]), "/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	groupName := parts[0]
	key := parts[1]
	group := GetGroup(groupName)
	if group == nil {
		http.Error(w, "no such group:"+groupName, http.StatusNotFound)
		return
	}
	view, err := group.Get(key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
	//w.Header().Set("Content-Type", "application/octet-stream")
	//w.Write(view.ByteSlice())
	body, err := proto.Marshal(&pb.Response{Value: view.ByteSlice()})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(body)
}

//节点选择与 HTTP 客户端

//使用 http.Get() 方式获取返回值，并转换为 []bytes 类型。
//func (h *httpGetter) Get(group string, key string) ([]byte, error)
func (h *httpGetter) Get(in *pb.Request, out *pb.Response) error {
	//u := fmt.Sprintf("%v%v/%v", h.baseURL, url.QueryEscape(group), url.QueryEscape(key))
	//res, err := http.Get(u)
	u := fmt.Sprintf(
		"%v%v/%v",
		h.baseURL,
		url.QueryEscape(in.GetGroup()),
		url.QueryEscape(in.GetKey()),
	)
	res, err := http.Get(u)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned:%v", res.Status)
	}
	bytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("reading response body:%v", err)
	}
	//return bytes, nil
	if err = proto.Unmarshal(bytes, out); err != nil {
		return fmt.Errorf("decoding response body: %v", err)
	}

	return nil
}

var _ PeerGetter = (*httpGetter)(nil)

//实现 PeerPicker 接口

//Set() 方法实例化了一致性哈希算法，并且添加了传入的节点
func (p *HTTPPool) Set(peers ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peers = consistenthash.New(defaultReplicas, nil)
	p.peers.Add(peers...)

	//并为每一个节点创建了一个 HTTP 客户端 httpGetter
	p.httpGetters = make(map[string]*httpGetter, len(peers))
	for _, peer := range peers {
		p.httpGetters[peer] = &httpGetter{baseURL: peer + p.basePath}
	}
}

//PickerPeer() 包装了一致性哈希算法的 Get() 方法，根据具体的 key，选择节点，返回节点对应的 HTTP 客户端。
//HTTPPool 既具备了提供 HTTP 服务的能力，也具备了根据具体的 key，创建 HTTP 客户端从远程节点获取缓存值的能力。
func (p *HTTPPool) PickPeer(key string) (PeerGetter, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if peer := p.peers.Get(key); peer != "" && peer != p.self {
		p.Log("Pick peer %s", peer)
		return p.httpGetters[peer], true
	}
	return nil, false
}

var _ PeerPicker = (*HTTPPool)(nil)
