package kcp

import (
	"encoding/binary"
	"sync/atomic"
	"time"
)

/**
 * 引用：https://blog.csdn.net/lixiaowei16/article/details/90485157
 * 		http://vearne.cc/archives/39317
 * 		https://github.com/shaoyuan1943/gokcp/blob/master/kcp.go
 *		https://github.com/xtaci/kcp-go
 *
 * 0               4   5   6       8 (BYTE)
 * +---------------+---+---+-------+
 * |     conv      |cmd|frg|  wnd  |
 * +---------------+---+---+-------+   8
 * |     ts        |     sn        |
 * +---------------+---------------+  16
 * |     una       |     len       |
 * +---------------+---------------+  24
 * |                               |
 * |        DATA (optional)        |
 * |                               |
 * +-------------------------------+
 * conv:连接号。UDP是无连接的，conv用于表示来自于哪个客户端。对连接的一种替代, 因为有conv, 所以KCP也是支持多路复用的
 * cmd:命令类型，只有四种:
 * 				CMD四种类型
 *                     ----------------------------------------------------------------------------------
 * 					   |cmd 			|作用 					|备注                    				|
 *                     ----------------------------------------------------------------------------------
 * 					   |IKCP_CMD_PUSH 	|数据推送命令            |与IKCP_CMD_ACK对应      				|
 *                     ----------------------------------------------------------------------------------
 * 					   |IKCP_CMD_ACK 	|确认命令                |1、RTO更新，2、确认发送包接收方已接收到	|
 *                     ----------------------------------------------------------------------------------
 * 					   |IKCP_CMD_WASK 	|接收窗口大小询问命令     |与IKCP_CMD_WINS对应      				|
 *                     ----------------------------------------------------------------------------------
 * 					   |IKCP_CMD_WINS 	|接收窗口大小告知命令     |                        				|
 *                     ----------------------------------------------------------------------------------
 * frg:分片，用户数据可能会被分成多个KCP包，发送出去
 * wnd:接收窗口大小，发送方的发送窗口不能超过接收方给出的数值
 * ts: 时间序列
 * sn: 序列号
 * una:下一个可接收的序列号。其实就是确认号，收到sn=10的包，una为11
 * len:数据长度(DATA的长度)
 * data:用户数据
 *
 * KCP流程说明
 * 1. 判定是消息模式还是流模式
 * 		1.1 消息模式(不拆包): 对应传输--文本消息
 * 			KCP header        KCP DATA
 *       -----------------------------------------------------------------------------------------------------------------------
 *       |  sn  |  90  |  --------------------  |  sn  |  91  |  --------------------  |  sn  |  92  |  --------------------   |
 *       |  frg |  0   |  |       MSG1       |  |  frg |  0   |  |       MSG2       |  |  frg |  0   |  |       MSG3       |   |
 *       |  len |  15  |  --------------------  |  sn  |  20  |  --------------------  |  sn  |  18  |  --------------------   |
 *       -----------------------------------------------------------------------------------------------------------------------
 * 		1.2 消息模式(拆包；拆包原则：根据MTU(最大传输单元决定))：对应传输--图片或文件消息
 * 			KCP header        KCP DATA
 *       -----------------------------------------------------------------------------------------------------------------------
 *       |  sn  |  90  |  --------------------  |  sn  |  91  |  --------------------  |  sn  |  92  |  --------------------   |
 *       |  frg |  2   |  |      MSG4-1      |  |  frg |  1   |  |      MSG4-2      |  |  frg |  0   |  |      MSG4-3      |   |
 *       |  len | 1376 |  --------------------  |  sn  | 1376 |  --------------------  |  sn  |  500 |  --------------------   |
 *       -----------------------------------------------------------------------------------------------------------------------
 * 		Msg被拆成了3部分，包含在3个KCP包中。注意, frg的序号是从大到小的，一直到0为止。这样接收端收到KCP包时，只有拿到frg为0的包，才会进行组装并交付给上层应用程序。
 *         由于frg在header中占1个字节，也就是最大能支持（1400 – 24[所有头的长度]） * 256 / 1024 = 344kB的消息
 * 		1.3 流模式
 * 			KCP header        KCP DATA
 *       ---------------------------------------------------------------------------------
 *       |  sn  |  90  |  -------  ------  --------  |  sn  |  91  |  --------------------
 *       |  frg |  0   |  | MSG1| | MSG2| | MSG3-1|  |  frg |  0   |  |       MSG3-2     |
 *       |  len | 1376 |  ------  ------  ---------  |  sn  |  53  |  --------------------
 *       ---------------------------------------------------------------------------------
 *      1.4 消息模式与流模式对比
 * 			1.4.1 消息模式：	减少了上层应用拆分数据流的麻烦，但是对网络的利用率较低。Payload(有效载荷)少，KCP头占比过大。
 * 			1.4.2 流模式：	KCP试图让每个KCP包尽可能装满。一个KCP包中可能包含多个消息。但是上层应用需要自己来判断每个消息的边界。
 * 2. 根据不同模式，得到需要传输的数据片段(segment)
 * 3. 数据片段在发送端会存储在snd_queue中
 * -----------------进入发送、传输、接收阶段------------------------------------------------------------------
 * 		-------------------------									-------------------------
 * 		|	Sender				|									|	Receiver			|
 * 		|		 ------------	|									|		 ------------	|
 *		|		|	APP		|	|									|		|	APP		|	|
 *		|		------------	|									|		------------	|
 *		|		    \|/		|										|		    /|\			|
 * 		|		------------	|									|		------------	|
 *		|		|  send buf	|---|--------------- LINK --------------|----->	|receive buf|	|
 *		|		------------	|									|		------------	|
 * 		-------------------------									-------------------------
 * 		此过程中的三个阶段分别对应一个容量(发送阶段：IKCP_WND_SND，传输阶段：cwnd，接受阶段：IKCP_WND_RCV)
 * 		cwnd剩余容量：snd_nxt(计划要发送的下一个segment号)-snd_una(接受端已经确认的最大连续接受号)=LINK中途在传输的数据量
 * 4. 需要发送的时候，会将数据转移到snd_buf；但是snd_buf大小是有限的，所以在转移之前需要判定snd_buf的大小
 *		4.1 snd_buf未满：将snd_queue的数据块flush到snd_buf，知道snd_queue清空或snd_buf已满停止
 * 		4.2 snd_buf已满、cwnd未满: 将snd_buf的数据send出去，然后将snd_queue写入snd_buf
 * 		4.3 snd_buf已满、cwnd已满: 不在写入，通过一个定时器定时判断cwnd是否有剩余流量，如果存在，完成第二步操作
 * 		4.4 其他情况：
 * 			4.4.1 如果snd_queue已满，snd_buf已满，cwnd已满，数据依然会直接flush，如果失败，即算超时处理
 *			4.4.2 如果NoDelay=false，就不在写入snd_buf
 * 5. LINK: 回调UDP的send进行传输(kcp->output=udp_output)
 * 6. 接受数据:
 * 		6.1 数据发送给接受端，接受端通过input进行接受，并解包receive buf，有确认的数据包recv_buf-->recv_queue
 *		6.2 获取完整用户数据：从receive queue中进行拼装完整用户数据
 */

const (
	IKCP_RTO_NDL     = 30  // no delay min rto
	IKCP_RTO_MIN     = 100 // normal min rto
	IKCP_RTO_DEF     = 200
	IKCP_RTO_MAX     = 60000
	IKCP_CMD_PUSH    = 81 // cmd: push data 数据推送命令
	IKCP_CMD_ACK     = 82 // cmd: ack		确认命令
	IKCP_CMD_WASK    = 83 // cmd: window probe (ask)	接收窗口大小询问命令
	IKCP_CMD_WINS    = 84 // cmd: window size (tell)	接收窗口大小告知命令
	IKCP_ASK_SEND    = 1  // need to send IKCP_CMD_WASK	表示请求远端告知窗口大小
	IKCP_ASK_TELL    = 2  // need to send IKCP_CMD_WINS 表示告知远端窗口大小
	IKCP_WND_SND     = 32 // send win size			发送窗口大小
	IKCP_WND_RCV     = 32
	IKCP_MTU_DEF     = 1400 // 最大传输单元 MSS(最大片段尺寸)=MTU-OVERHEAD
	IKCP_ACK_FAST    = 3
	IKCP_INTERVAL    = 100    // 最小重传时间(单位ms)
	IKCP_OVERHEAD    = 24     // 头信息大小
	IKCP_DEADLINK    = 20     // 最大重传次数
	IKCP_THRESH_INIT = 2      //	塞窗口初始值
	IKCP_THRESH_MIN  = 2      //  拥塞窗口最小值
	IKCP_PROBE_INIT  = 7000   // 7 secs to probe window size
	IKCP_PROBE_LIMIT = 120000 // up to 120 secs to probe window
	IKCP_SN_OFFSET   = 12
)

// monotonic reference time point
var refTime time.Time = time.Now()

// currentMs returns current elapsed monotonic milliseconds since program startup
func currentMs() uint32 { return uint32(time.Since(refTime) / time.Millisecond) }

// output_callback is a prototype which ought capture conn and call conn.Write
type output_callback func(buf []byte, size int)

/* encode 8 bits unsigned int */
func ikcp_encode8u(p []byte, c byte) []byte {
	p[0] = c
	return p[1:]
}

/* decode 8 bits unsigned int */
func ikcp_decode8u(p []byte, c *byte) []byte {
	*c = p[0]
	return p[1:]
}

/* encode 16 bits unsigned int (lsb) */
func ikcp_encode16u(p []byte, w uint16) []byte {
	binary.LittleEndian.PutUint16(p, w)
	return p[2:]
}

/* decode 16 bits unsigned int (lsb) */
func ikcp_decode16u(p []byte, w *uint16) []byte {
	*w = binary.LittleEndian.Uint16(p)
	return p[2:]
}

/* encode 32 bits unsigned int (lsb) */
func ikcp_encode32u(p []byte, l uint32) []byte {
	binary.LittleEndian.PutUint32(p, l)
	return p[4:]
}

/* decode 32 bits unsigned int (lsb) */
func ikcp_decode32u(p []byte, l *uint32) []byte {
	*l = binary.LittleEndian.Uint32(p)
	return p[4:]
}

func _imin_(a, b uint32) uint32 {
	if a <= b {
		return a
	}
	return b
}

func _imax_(a, b uint32) uint32 {
	if a >= b {
		return a
	}
	return b
}

func _ibound_(lower, middle, upper uint32) uint32 {
	return _imin_(_imax_(lower, middle), upper)
}

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

// segment defines a KCP segment
type segment struct {
	conv     uint32
	cmd      uint8
	frg      uint8
	wnd      uint16 //接收窗口大小
	ts       uint32 //时间序列
	sn       uint32 //序列号
	una      uint32 //下一个可接收的序列号，其实就是确认号
	rto      uint32 //由ack接收延迟计算出来的重传超时时间
	xmit     uint32 //发送分片的次数，每发送一次加一
	resendts uint32 //下次超时重传的时间戳
	fastack  uint32 //收到ack时计算的该分片被跳过的累计次数
	acked    uint32 // mark if the seg has acked
	data     []byte
}

// encode a segment into buffer
func (seg *segment) encode(ptr []byte) []byte {
	ptr = ikcp_encode32u(ptr, seg.conv)
	ptr = ikcp_encode8u(ptr, seg.cmd)
	ptr = ikcp_encode8u(ptr, seg.frg)
	ptr = ikcp_encode16u(ptr, seg.wnd)
	ptr = ikcp_encode32u(ptr, seg.ts)
	ptr = ikcp_encode32u(ptr, seg.sn)
	ptr = ikcp_encode32u(ptr, seg.una)
	ptr = ikcp_encode32u(ptr, uint32(len(seg.data)))
	atomic.AddUint64(&DefaultSnmp.OutSegs, 1)
	return ptr
}

// KCP defines a single KCP connection
type KCP struct {
	conv, mtu, mss, state                  uint32 // conv:客户端来源id； mtu：最大传输单元(默认1400)；mss: 最大片段尺寸MTU-OVERHEAD；state: 连接状态（0xFFFFFFFF表示断开连接）
	snd_una, snd_nxt, rcv_nxt              uint32 // snd_una: 第一个未确认的包; snd_nxt：下一个要发送的sn; rcv_nxt：下一个应该接受的sn
	ssthresh                               uint32 // 拥塞窗口阈值
	rx_rttvar, rx_srtt                     int32  // RTT的平均偏差; RTT的一个加权RTT平均值，平滑值; RTT: 一个报文段发送出去，到收到对应确认包的时间差。
	rx_rto, rx_minrto                      uint32 // 由ack接收延迟计算出来的重传超时时间(重传超时时间)，最小重传超时时间
	snd_wnd, rcv_wnd, rmt_wnd, cwnd, probe uint32 // 发送窗口；接收窗口；远程窗口(发送端获取的接受端尺寸)；拥塞窗口大小(传输窗口)；探查变量
	interval, ts_flush                     uint32 // 内部flush刷新间隔;  下次flush刷新时间戳
	nodelay, updated                       uint32 // 是否启动无延迟模式：0不需要，1需要；updated: 是否调用过update函数的标识
	ts_probe, probe_wait                   uint32 // 窗口探查时间，窗口探查时间间隔
	dead_link, incr                        uint32 // 最大重传次数; 可发送的最大数据量

	fastresend     int32 // 重传
	nocwnd, stream int32 // 取消拥塞控制，是否是流模式

	snd_queue, snd_buf []segment // 发送队列; 发送buf
	rcv_queue, rcv_buf []segment // 接收队列; 接收buf

	acklist []ackItem // 待发送的ack列表(包含sn与ts)  |sn0|ts0|sn1|ts1|... 形式存在

	buffer   []byte          // 存储消息字节流的内存
	reserved int             // 不同协议保留位数
	output   output_callback // 回调函数
}

type ackItem struct {
	sn uint32
	ts uint32
}

// NewKCP create a new kcp state machine
//
// 'conv' must be equal in the connection peers, or else data will be silently rejected.
//
// 'output' function will be called whenever these is data to be sent on wire.
func NewKCP(conv uint32, output output_callback) *KCP {
	kcp := new(KCP)
	kcp.conv = conv
	kcp.snd_wnd = IKCP_WND_SND
	kcp.rcv_wnd = IKCP_WND_RCV
	kcp.rmt_wnd = IKCP_WND_RCV
	kcp.mtu = IKCP_MTU_DEF
	kcp.mss = kcp.mtu - IKCP_OVERHEAD
	kcp.buffer = make([]byte, kcp.mtu)
	kcp.rx_rto = IKCP_RTO_DEF
	kcp.rx_minrto = IKCP_RTO_MIN
	kcp.interval = IKCP_INTERVAL
	kcp.ts_flush = IKCP_INTERVAL
	kcp.ssthresh = IKCP_THRESH_INIT
	kcp.dead_link = IKCP_DEADLINK
	kcp.output = output
	return kcp
}

// newSegment creates a KCP segment
func (kcp *KCP) newSegment(size int) (seg segment) {
	seg.data = xmitBuf.Get().([]byte)[:size]
	return
}

// delSegment recycles a KCP segment
func (kcp *KCP) delSegment(seg *segment) {
	if seg.data != nil {
		xmitBuf.Put(seg.data)
		seg.data = nil
	}
}

// ReserveBytes keeps n bytes untouched from the beginning of the buffer,
// the output_callback function should be aware of this.
//
// Return false if n >= mss
func (kcp *KCP) ReserveBytes(n int) bool {
	if n >= int(kcp.mtu-IKCP_OVERHEAD) || n < 0 {
		return false
	}
	kcp.reserved = n
	kcp.mss = kcp.mtu - IKCP_OVERHEAD - uint32(n)
	return true
}

// PeekSize checks the size of next message in the recv queue
func (kcp *KCP) PeekSize() (length int) {
	if len(kcp.rcv_queue) == 0 {
		return -1
	}

	seg := &kcp.rcv_queue[0]
	if seg.frg == 0 {
		return len(seg.data)
	}

	if len(kcp.rcv_queue) < int(seg.frg+1) {
		return -1
	}

	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		length += len(seg.data)
		if seg.frg == 0 {
			break
		}
	}
	return
}

// Send is user/upper level send, returns below zero for error
// * Send 数据格式化--> Segment
// * 1. 先判断消息类型；如果是流模式，则判断是否与现有Segment合并
// * 2. 如果是消息模式，则创建新Segment
func (kcp *KCP) Send(buffer []byte) int {
	var count int         // 表示需要增加几个Segment才能将新数据存放下
	if len(buffer) == 0 { //如果数据为空，直接退出
		return -1
	}

	// append to previous segment in streaming mode (if possible)
	// 如果是流模式，则判断是否与现有Segment合并
	if kcp.stream != 0 {
		// 判断是否有未发送的消息
		// 此处只判定snd_queue，因为snd_buf中可能是已经发送，只不过未确认而没有删除的记录
		n := len(kcp.snd_queue)
		if n > 0 {
			seg := &kcp.snd_queue[n-1]        //将最后一条记录取出
			if len(seg.data) < int(kcp.mss) { // 如果还有剩余空间
				capacity := int(kcp.mss) - len(seg.data) // 获取剩余空间
				extend := capacity                       // 初始化扩展大小，默认是全部占用
				if len(buffer) < capacity {              // 如果新数据小于剩余空间
					extend = len(buffer) // 则设置扩展大小为新数据长度
				}

				// grow slice, the underlying cap is guaranteed to
				// be larger than kcp.mss
				oldlen := len(seg.data)             //	原片段数据长度
				seg.data = seg.data[:oldlen+extend] // 扩展数据块位置
				copy(seg.data[oldlen:], buffer)     // 将buffer拷贝到原有数据后面
				buffer = buffer[extend:]            // 更新buffer为未完全放入到此Segment中的内容
			}
		}

		if len(buffer) == 0 { // 如果buffer剩余的内容长度为0，表示已经完成数据的写入Segment的过程，结束
			return 0
		}
	}

	// 以下说明中，无论是已经合并一部分到上一个Segment还是没有合并的情况，统统称呼为新数据
	if len(buffer) <= int(kcp.mss) { // 如果新数据小于一个新Segment的数据长度
		count = 1
	} else {
		count = (len(buffer) + int(kcp.mss) - 1) / int(kcp.mss)
	}

	// 如果新增加的输入大于255个，直接表示无法发送  254*(1400-24)/1024=344KB的数据
	// 想要多发数据，只能通过MTU进行设定
	if count > 255 {
		return -2
	}

	if count == 0 {
		count = 1
	}

	for i := 0; i < count; i++ { // 遍历Segment数量
		var size int
		if len(buffer) > int(kcp.mss) { // 判断剩余的buffer长度是否还是大于Segment存放数据的空间
			size = int(kcp.mss)
		} else {
			size = len(buffer)
		}
		seg := kcp.newSegment(size)   // 创建Segment
		copy(seg.data, buffer[:size]) // 将数据存放到新的Segment中
		if kcp.stream == 0 {          // message mode 如果是消息模式，则需要将frg标记号码，从大到小
			seg.frg = uint8(count - i - 1)
		} else { // stream mode	如果是流模式，就不需要
			seg.frg = 0
		}
		kcp.snd_queue = append(kcp.snd_queue, seg) // 将Segment存放到snd_queue中去
		buffer = buffer[size:]                     // 将数据更新为剩余数据
	}
	return 0
}

// flush pending data
/**
 * 4. 需要发送的时候，会将数据转移到snd_buf；但是snd_buf大小是有限的，所以在转移之前需要判定snd_buf的大小
 * ackOnly=true  更新acklist(待发送列表)
 * ackOnly=false 更新acklist(待发送列表), 将snd_queue移动到snd_buf，确认重传数据及时间
 * 通过kcp.output:发送数据
 */
func (kcp *KCP) flush(ackOnly bool) uint32 {
	var seg segment
	seg.conv = kcp.conv
	seg.cmd = IKCP_CMD_ACK
	seg.wnd = kcp.wnd_unused()
	seg.una = kcp.rcv_nxt

	buffer := kcp.buffer         // 默认情况buffer=mtu的大小
	ptr := buffer[kcp.reserved:] // keep n bytes untouched 默认情况ptr等于mtu-kcp.reserved

	// makeSpace makes room for writing
	makeSpace := func(space int) {
		size := len(buffer) - len(ptr)
		if size+space > int(kcp.mtu) {
			kcp.output(buffer, size)
			ptr = buffer[kcp.reserved:]
		}
	}

	// 调用底层通讯协议，发送数据，比例UDP协议
	// flush bytes in buffer if there is any
	flushBuffer := func() {
		size := len(buffer) - len(ptr) // 如果没有数据扩充，size=kcp.reserved
		if size > kcp.reserved {       // 判断buffer是否扩充
			kcp.output(buffer, size) // 调用回调函数进行发送，数据会发送到接收端--对于数据源是来自buffer，而不是snd_buf，不理解
		}
	}

	// flush acknowledges
	for i, ack := range kcp.acklist {
		makeSpace(IKCP_OVERHEAD)
		// filter jitters caused by bufferbloat
		if _itimediff(ack.sn, kcp.rcv_nxt) >= 0 || len(kcp.acklist)-1 == i {
			seg.sn, seg.ts = ack.sn, ack.ts
			ptr = seg.encode(ptr)
		}
	}
	kcp.acklist = kcp.acklist[0:0]

	// 仅仅更新acklist
	if ackOnly { // flash remain ack segments
		flushBuffer()
		return kcp.interval
	}

	// probe window size (if remote window size equals zero)
	if kcp.rmt_wnd == 0 { // 如果远程端口尺寸==0
		current := currentMs()
		if kcp.probe_wait == 0 { // 如果探查等待时间也没有设定
			kcp.probe_wait = IKCP_PROBE_INIT        // 则7s后进行窗口尺寸探查
			kcp.ts_probe = current + kcp.probe_wait // 探查时间为当前时间+7s
		} else {
			if _itimediff(current, kcp.ts_probe) >= 0 { // 判断当前时间已经到达窗口探查时间
				if kcp.probe_wait < IKCP_PROBE_INIT {
					kcp.probe_wait = IKCP_PROBE_INIT
				}
				kcp.probe_wait += kcp.probe_wait / 2   // 探查时间扩充一半
				if kcp.probe_wait > IKCP_PROBE_LIMIT { // 如果新的扩充时间大于120s
					kcp.probe_wait = IKCP_PROBE_LIMIT
				}
				kcp.ts_probe = current + kcp.probe_wait // 更新下一次探查时间
				kcp.probe |= IKCP_ASK_SEND              // 设置此次为需要探查
			}
		}
	} else {
		kcp.ts_probe = 0 // 如果知道远程端口尺寸，则无需设定探查时间与下次探查时间
		kcp.probe_wait = 0
	}

	// flush window probing commands
	if (kcp.probe & IKCP_ASK_SEND) != 0 { // 如果需要探查，设定命令字
		seg.cmd = IKCP_CMD_WASK
		makeSpace(IKCP_OVERHEAD)
		ptr = seg.encode(ptr)
	}

	// flush window probing commands
	if (kcp.probe & IKCP_ASK_TELL) != 0 { // 如果是接受端回复
		seg.cmd = IKCP_CMD_WINS
		makeSpace(IKCP_OVERHEAD)
		ptr = seg.encode(ptr)
	}

	kcp.probe = 0

	// calculate window size
	cwnd := _imin_(kcp.snd_wnd, kcp.rmt_wnd) // 传输窗口是发送端与接收端中较小的一个
	if kcp.nocwnd == 0 {                     // 如果未取消拥塞控制
		cwnd = _imin_(kcp.cwnd, cwnd) // 正在发送的数据大小与窗口尺寸比较
	}

	// sliding window, controlled by snd_nxt && sna_una+cwnd
	newSegsCount := 0
	for k := range kcp.snd_queue {
		// kcp.snd_nxt[待发送的sn]-(kcp.snd_una[已经确认的连续sn号]+cwnd[发送途中的数量])>=0
		// 表示发送中+未确认的已经大于cwnd的尺寸了，需要等待
		if _itimediff(kcp.snd_nxt, kcp.snd_una+cwnd) >= 0 {
			break
		}
		newseg := kcp.snd_queue[k]                // 从开始位置取数据
		newseg.conv = kcp.conv                    // 客户端号，一个客户端的号码在同一次启动中是唯一的
		newseg.cmd = IKCP_CMD_PUSH                // 数据推出命令字
		newseg.sn = kcp.snd_nxt                   // 设定
		kcp.snd_buf = append(kcp.snd_buf, newseg) // 将数据移动到snd_buf中去
		kcp.snd_nxt++
		newSegsCount++
	}
	if newSegsCount > 0 { // 如果有将snd_queue中的数据移动至snd_buf中，则需要将snd_queue中的对应数据删除
		kcp.snd_queue = kcp.remove_front(kcp.snd_queue, newSegsCount)
	}

	////////////////// 设置重传策略 ///////////////////////////////
	// calculate resent
	resent := uint32(kcp.fastresend)
	if kcp.fastresend <= 0 { // 如果触发快速重传的重复ack个数 <= 0
		resent = 0xffffffff
	}

	// check for retransmissions
	current := currentMs()
	// 新变更传输状态的数量，丢失片段数，快速重发片段数，提前重发片段数
	var change, lostSegs, fastRetransSegs, earlyRetransSegs uint64
	minrto := int32(kcp.interval)

	// 确定核查边界
	ref := kcp.snd_buf[:len(kcp.snd_buf)] // for bounds check elimination
	for k := range ref {                  // 将确定要检查的内容进行遍历
		segment := &ref[k]
		needsend := false
		if segment.acked == 1 { // 如果此记录以及被确认过
			continue
		}
		if segment.xmit == 0 { // initial transmit 如果传输状态为0--> 未发送
			needsend = true
			segment.rto = kcp.rx_rto                 // 重传超时时间
			segment.resendts = current + segment.rto // 发送时间
		} else if segment.fastack >= resent { // fast retransmit 快速确认act数>快速重传数
			needsend = true
			segment.fastack = 0
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
			change++
			fastRetransSegs++
		} else if segment.fastack > 0 && newSegsCount == 0 { // early retransmit 快速确认数>0且没有新数据加入
			needsend = true
			segment.fastack = 0
			segment.rto = kcp.rx_rto
			segment.resendts = current + segment.rto
			change++
			earlyRetransSegs++
		} else if _itimediff(current, segment.resendts) >= 0 { // RTO  当前时间已经达到重传时间的
			needsend = true
			if kcp.nodelay == 0 { // 如果不启动无延迟模式(延迟)；当前Segment的每一次重传时间加rx_rto
				segment.rto += kcp.rx_rto
			} else {
				segment.rto += kcp.rx_rto / 2
			}
			segment.fastack = 0
			segment.resendts = current + segment.rto
			lostSegs++
		}

		if needsend {
			current = currentMs()
			segment.xmit++ // segment重传次数
			segment.ts = current
			segment.wnd = seg.wnd
			segment.una = seg.una

			need := IKCP_OVERHEAD + len(segment.data)
			makeSpace(need)
			ptr = segment.encode(ptr)
			copy(ptr, segment.data)
			ptr = ptr[len(segment.data):]

			if segment.xmit >= kcp.dead_link { // 最大重传次数
				kcp.state = 0xFFFFFFFF // 连接状态（0xFFFFFFFF表示断开连接）
			}
		}

		// get the nearest rto
		if rto := _itimediff(segment.resendts, current); rto > 0 && rto < minrto { // 如果重传时间已到，并且重传时间小于最小重传时间
			minrto = rto
		}
	}

	// flash remain segments
	flushBuffer()

	// 计数
	// counter updates
	sum := lostSegs
	if lostSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.LostSegs, lostSegs)
	}
	if fastRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.FastRetransSegs, fastRetransSegs)
		sum += fastRetransSegs
	}
	if earlyRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.EarlyRetransSegs, earlyRetransSegs)
		sum += earlyRetransSegs
	}
	if sum > 0 {
		atomic.AddUint64(&DefaultSnmp.RetransSegs, sum)
	}

	// cwnd update
	if kcp.nocwnd == 0 { // 如果未取消拥塞控制
		// update ssthresh
		// rate halving, https://tools.ietf.org/html/rfc6937
		if change > 0 {
			inflight := kcp.snd_nxt - kcp.snd_una // 待发送的与未确认最小值得差值==未确认的总数
			kcp.ssthresh = inflight / 2
			if kcp.ssthresh < IKCP_THRESH_MIN { // 如果 拥塞窗口阈值 小于 拥塞窗口最小值
				kcp.ssthresh = IKCP_THRESH_MIN
			}
			kcp.cwnd = kcp.ssthresh + resent // 拥塞窗口阈值+重传数量
			kcp.incr = kcp.cwnd * kcp.mss    // 可发送的最大数据量=拥塞窗口*最大分片大小
		}

		// congestion control, https://tools.ietf.org/html/rfc5681
		if lostSegs > 0 {
			kcp.ssthresh = cwnd / 2
			if kcp.ssthresh < IKCP_THRESH_MIN {
				kcp.ssthresh = IKCP_THRESH_MIN
			}
			kcp.cwnd = 1
			kcp.incr = kcp.mss
		}

		if kcp.cwnd < 1 {
			kcp.cwnd = 1
			kcp.incr = kcp.mss
		}
	}

	return uint32(minrto)
}

// Recv
/**
 * 6.2 获取完整用户数据：从receive queue中进行拼装完整用户数据
 * 1. 遍历rcv_queue中，是否可以获取到连续块却最后一块的frg=0的情况，如果存在，则提取一个完整用户数据；并删除rcv_queue中对应的数据块，释放空间
 * 2. 提取完成后，遍历rcv_buf中，是否还有有效数据，更新到rcv_queue中；等待下次进行判断
 */
// Receive data from kcp state machine
//
// Return number of bytes read.
//
// Return -1 when there is no readable data.
//
// Return -2 if len(buffer) is smaller than kcp.PeekSize().
func (kcp *KCP) Recv(buffer []byte) (n int) {
	peeksize := kcp.PeekSize()
	if peeksize < 0 {
		return -1
	}

	if peeksize > len(buffer) {
		return -2
	}

	var fast_recover bool
	if len(kcp.rcv_queue) >= int(kcp.rcv_wnd) {
		fast_recover = true
	}

	// merge fragment
	count := 0
	for k := range kcp.rcv_queue { // 如果frg=0,则表示是一个分拆的数据中的最后一个数据块
		seg := &kcp.rcv_queue[k]
		copy(buffer, seg.data)
		buffer = buffer[len(seg.data):]
		n += len(seg.data)
		count++
		kcp.delSegment(seg)
		if seg.frg == 0 {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = kcp.remove_front(kcp.rcv_queue, count)
	}
	// 释放了部分rcv_queue空间后，判断rcv_buf中，是否还有有效数据需要更新到rcv_queue中
	// move available data from rcv_buf -> rcv_queue
	count = 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.sn == kcp.rcv_nxt && len(kcp.rcv_queue)+count < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	// fast recover
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) && fast_recover {
		// ready to send back IKCP_CMD_WINS in ikcp_flush
		// tell remote my window size
		kcp.probe |= IKCP_ASK_TELL
	}
	return
}

func (kcp *KCP) update_ack(rtt int32) {
	// https://tools.ietf.org/html/rfc6298
	var rto uint32
	if kcp.rx_srtt == 0 {
		kcp.rx_srtt = rtt
		kcp.rx_rttvar = rtt >> 1
	} else {
		delta := rtt - kcp.rx_srtt
		kcp.rx_srtt += delta >> 3
		if delta < 0 {
			delta = -delta
		}
		if rtt < kcp.rx_srtt-kcp.rx_rttvar {
			// if the new RTT sample is below the bottom of the range of
			// what an RTT measurement is expected to be.
			// give an 8x reduced weight versus its normal weighting
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 5
		} else {
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 2
		}
	}
	rto = uint32(kcp.rx_srtt) + _imax_(kcp.interval, uint32(kcp.rx_rttvar)<<2)
	kcp.rx_rto = _ibound_(kcp.rx_minrto, rto, IKCP_RTO_MAX)
}

func (kcp *KCP) shrink_buf() {
	if len(kcp.snd_buf) > 0 {
		seg := &kcp.snd_buf[0]
		kcp.snd_una = seg.sn
	} else {
		kcp.snd_una = kcp.snd_nxt
	}
}

func (kcp *KCP) parse_ack(sn uint32) {
	if _itimediff(sn, kcp.snd_una) < 0 || _itimediff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if sn == seg.sn {
			// mark and free space, but leave the segment here,
			// and wait until `una` to delete this, then we don't
			// have to shift the segments behind forward,
			// which is an expensive operation for large window
			seg.acked = 1
			kcp.delSegment(seg)
			break
		}
		if _itimediff(sn, seg.sn) < 0 {
			break
		}
	}
}

func (kcp *KCP) parse_fastack(sn, ts uint32) {
	if _itimediff(sn, kcp.snd_una) < 0 || _itimediff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if _itimediff(sn, seg.sn) < 0 {
			break
		} else if sn != seg.sn && _itimediff(seg.ts, ts) <= 0 {
			seg.fastack++
		}
	}
}

func (kcp *KCP) parse_una(una uint32) int {
	count := 0
	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if _itimediff(una, seg.sn) > 0 {
			kcp.delSegment(seg)
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.snd_buf = kcp.remove_front(kcp.snd_buf, count)
	}
	return count
}

// ack append
func (kcp *KCP) ack_push(sn, ts uint32) {
	kcp.acklist = append(kcp.acklist, ackItem{sn, ts})
}

// returns true if data has repeated
func (kcp *KCP) parse_data(newseg segment) bool {
	sn := newseg.sn
	if _itimediff(sn, kcp.rcv_nxt+kcp.rcv_wnd) >= 0 ||
		_itimediff(sn, kcp.rcv_nxt) < 0 {
		return true
	}

	n := len(kcp.rcv_buf) - 1
	insert_idx := 0
	repeat := false
	for i := n; i >= 0; i-- {
		seg := &kcp.rcv_buf[i]
		if seg.sn == sn {
			repeat = true
			break
		}
		if _itimediff(sn, seg.sn) > 0 {
			insert_idx = i + 1
			break
		}
	}

	if !repeat {
		// replicate the content if it's new
		dataCopy := xmitBuf.Get().([]byte)[:len(newseg.data)]
		copy(dataCopy, newseg.data)
		newseg.data = dataCopy

		if insert_idx == n+1 {
			kcp.rcv_buf = append(kcp.rcv_buf, newseg)
		} else {
			kcp.rcv_buf = append(kcp.rcv_buf, segment{})
			copy(kcp.rcv_buf[insert_idx+1:], kcp.rcv_buf[insert_idx:])
			kcp.rcv_buf[insert_idx] = newseg
		}
	}

	// move available data from rcv_buf -> rcv_queue
	count := 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.sn == kcp.rcv_nxt && len(kcp.rcv_queue)+count < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	return repeat
}

// Input a packet into kcp state machine.
//
// 'regular' indicates it's a real data packet from remote, and it means it's not generated from ReedSolomon
// codecs.
//
// 'ackNoDelay' will trigger immediate ACK, but surely it will not be efficient in bandwidth
/**
 * 6.1 数据发送给接受端，接受端通过input进行接受，并解包receive buf，有确认的数据包recv_buf-->recv_queue
 * 1. 解包
 * 2. 确认是否有新的确认块，确认块需要重recv_buf-->recv_queue
 * 3. 更新相关参数，主要是是否有确认过未删除，recv_buf，recv_queue的尺寸
 */
func (kcp *KCP) Input(data []byte, regular, ackNoDelay bool) int {
	snd_una := kcp.snd_una
	if len(data) < IKCP_OVERHEAD {
		return -1
	}

	var latest uint32 // the latest ack packet
	var flag int
	var inSegs uint64
	var windowSlides bool

	for {
		var ts, sn, length, una, conv uint32
		var wnd uint16
		var cmd, frg uint8

		if len(data) < int(IKCP_OVERHEAD) {
			break
		}

		data = ikcp_decode32u(data, &conv)
		if conv != kcp.conv {
			return -1
		}

		data = ikcp_decode8u(data, &cmd)
		data = ikcp_decode8u(data, &frg)
		data = ikcp_decode16u(data, &wnd)
		data = ikcp_decode32u(data, &ts)
		data = ikcp_decode32u(data, &sn)
		data = ikcp_decode32u(data, &una)
		data = ikcp_decode32u(data, &length)
		if len(data) < int(length) {
			return -2
		}

		if cmd != IKCP_CMD_PUSH && cmd != IKCP_CMD_ACK &&
			cmd != IKCP_CMD_WASK && cmd != IKCP_CMD_WINS {
			return -3
		}

		// only trust window updates from regular packets. i.e: latest update
		if regular {
			kcp.rmt_wnd = uint32(wnd)
		}
		if kcp.parse_una(una) > 0 {
			windowSlides = true
		}
		kcp.shrink_buf()

		if cmd == IKCP_CMD_ACK {
			kcp.parse_ack(sn)
			kcp.parse_fastack(sn, ts)
			flag |= 1
			latest = ts
		} else if cmd == IKCP_CMD_PUSH {
			repeat := true
			if _itimediff(sn, kcp.rcv_nxt+kcp.rcv_wnd) < 0 {
				kcp.ack_push(sn, ts)
				if _itimediff(sn, kcp.rcv_nxt) >= 0 {
					var seg segment
					seg.conv = conv
					seg.cmd = cmd
					seg.frg = frg
					seg.wnd = wnd
					seg.ts = ts
					seg.sn = sn
					seg.una = una
					seg.data = data[:length]     // delayed data copying
					repeat = kcp.parse_data(seg) //// 输入放入recv_buf,并确认是否有确认数据块放入到recv_queue
				}
			}
			if regular && repeat {
				atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			}
		} else if cmd == IKCP_CMD_WASK {
			// ready to send back IKCP_CMD_WINS in Ikcp_flush
			// tell remote my window size
			kcp.probe |= IKCP_ASK_TELL
		} else if cmd == IKCP_CMD_WINS {
			// do nothing
		} else {
			return -3
		}

		inSegs++
		data = data[length:]
	}
	atomic.AddUint64(&DefaultSnmp.InSegs, inSegs)

	// update rtt with the latest ts
	// ignore the FEC packet
	if flag != 0 && regular {
		current := currentMs()
		if _itimediff(current, latest) >= 0 {
			kcp.update_ack(_itimediff(current, latest))
		}
	}

	// cwnd update when packet arrived
	if kcp.nocwnd == 0 {
		if _itimediff(kcp.snd_una, snd_una) > 0 {
			if kcp.cwnd < kcp.rmt_wnd {
				mss := kcp.mss
				if kcp.cwnd < kcp.ssthresh {
					kcp.cwnd++
					kcp.incr += mss
				} else {
					if kcp.incr < mss {
						kcp.incr = mss
					}
					kcp.incr += (mss*mss)/kcp.incr + (mss / 16)
					if (kcp.cwnd+1)*mss <= kcp.incr {
						if mss > 0 {
							kcp.cwnd = (kcp.incr + mss - 1) / mss
						} else {
							kcp.cwnd = kcp.incr + mss - 1
						}
					}
				}
				if kcp.cwnd > kcp.rmt_wnd {
					kcp.cwnd = kcp.rmt_wnd
					kcp.incr = kcp.rmt_wnd * mss
				}
			}
		}
	}

	if windowSlides { // if window has slided, flush
		kcp.flush(false)
	} else if ackNoDelay && len(kcp.acklist) > 0 { // ack immediately
		kcp.flush(true)
	}
	return 0
}

func (kcp *KCP) wnd_unused() uint16 {
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) {
		return uint16(int(kcp.rcv_wnd) - len(kcp.rcv_queue))
	}
	return 0
}

// (deprecated)
//
// Update updates state (call it repeatedly, every 10ms-100ms), or you can ask
// ikcp_check when to call it again (without ikcp_input/_send calling).
// 'current' - current timestamp in millisec.
func (kcp *KCP) Update() {
	var slap int32

	current := currentMs()
	if kcp.updated == 0 {
		kcp.updated = 1
		kcp.ts_flush = current
	}

	slap = _itimediff(current, kcp.ts_flush)

	if slap >= 10000 || slap < -10000 {
		kcp.ts_flush = current
		slap = 0
	}

	if slap >= 0 {
		kcp.ts_flush += kcp.interval
		if _itimediff(current, kcp.ts_flush) >= 0 {
			kcp.ts_flush = current + kcp.interval
		}
		kcp.flush(false)
	}
}

// (deprecated)
//
// Check determines when should you invoke ikcp_update:
// returns when you should invoke ikcp_update in millisec, if there
// is no ikcp_input/_send calling. you can call ikcp_update in that
// time, instead of call update repeatly.
// Important to reduce unnacessary ikcp_update invoking. use it to
// schedule ikcp_update (eg. implementing an epoll-like mechanism,
// or optimize ikcp_update when handling massive kcp connections)
func (kcp *KCP) Check() uint32 {
	current := currentMs()
	ts_flush := kcp.ts_flush
	tm_flush := int32(0x7fffffff)
	tm_packet := int32(0x7fffffff)
	minimal := uint32(0)
	if kcp.updated == 0 {
		return current
	}

	if _itimediff(current, ts_flush) >= 10000 ||
		_itimediff(current, ts_flush) < -10000 {
		ts_flush = current
	}

	if _itimediff(current, ts_flush) >= 0 {
		return current
	}

	tm_flush = _itimediff(ts_flush, current)

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		diff := _itimediff(seg.resendts, current)
		if diff <= 0 {
			return current
		}
		if diff < tm_packet {
			tm_packet = diff
		}
	}

	minimal = uint32(tm_packet)
	if tm_packet >= tm_flush {
		minimal = uint32(tm_flush)
	}
	if minimal >= kcp.interval {
		minimal = kcp.interval
	}

	return current + minimal
}

// SetMtu changes MTU size, default is 1400
func (kcp *KCP) SetMtu(mtu int) int {
	if mtu < 50 || mtu < IKCP_OVERHEAD {
		return -1
	}
	if kcp.reserved >= int(kcp.mtu-IKCP_OVERHEAD) || kcp.reserved < 0 {
		return -1
	}

	buffer := make([]byte, mtu)
	if buffer == nil {
		return -2
	}
	kcp.mtu = uint32(mtu)
	kcp.mss = kcp.mtu - IKCP_OVERHEAD - uint32(kcp.reserved)
	kcp.buffer = buffer
	return 0
}

// NoDelay options
// fastest: ikcp_nodelay(kcp, 1, 20, 2, 1)
// nodelay: 0:disable(default), 1:enable
// interval: internal update timer interval in millisec, default is 100ms
// resend: 0:disable fast resend(default), 1:enable fast resend
// nc: 0:normal congestion control(default), 1:disable congestion control
func (kcp *KCP) NoDelay(nodelay, interval, resend, nc int) int {
	if nodelay >= 0 {
		kcp.nodelay = uint32(nodelay)
		if nodelay != 0 {
			kcp.rx_minrto = IKCP_RTO_NDL
		} else {
			kcp.rx_minrto = IKCP_RTO_MIN
		}
	}
	if interval >= 0 {
		if interval > 5000 {
			interval = 5000
		} else if interval < 10 {
			interval = 10
		}
		kcp.interval = uint32(interval)
	}
	if resend >= 0 {
		kcp.fastresend = int32(resend)
	}
	if nc >= 0 {
		kcp.nocwnd = int32(nc)
	}
	return 0
}

// WndSize sets maximum window size: sndwnd=32, rcvwnd=32 by default
func (kcp *KCP) WndSize(sndwnd, rcvwnd int) int {
	if sndwnd > 0 {
		kcp.snd_wnd = uint32(sndwnd)
	}
	if rcvwnd > 0 {
		kcp.rcv_wnd = uint32(rcvwnd)
	}
	return 0
}

// WaitSnd gets how many packet is waiting to be sent
func (kcp *KCP) WaitSnd() int {
	return len(kcp.snd_buf) + len(kcp.snd_queue)
}

// remove front n elements from queue
// if the number of elements to remove is more than half of the size.
// just shift the rear elements to front, otherwise just reslice q to q[n:]
// then the cost of runtime.growslice can always be less than n/2
func (kcp *KCP) remove_front(q []segment, n int) []segment {
	if n > cap(q)/2 {
		newn := copy(q, q[n:])
		return q[:newn]
	}
	return q[n:]
}

// Release all cached outgoing segments
func (kcp *KCP) ReleaseTX() {
	for k := range kcp.snd_queue {
		if kcp.snd_queue[k].data != nil {
			xmitBuf.Put(kcp.snd_queue[k].data)
		}
	}
	for k := range kcp.snd_buf {
		if kcp.snd_buf[k].data != nil {
			xmitBuf.Put(kcp.snd_buf[k].data)
		}
	}
	kcp.snd_queue = nil
	kcp.snd_buf = nil
}
