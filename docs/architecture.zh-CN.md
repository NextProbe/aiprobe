# 架构

*[English](architecture.md)*

AIProbe 是怎么从磁盘上的一个会话文件走到一份报告的，以及哪些设计决策是承重的。

写一个 adapter 需要做什么，见
[CONTRIBUTING.zh-CN.md](../CONTRIBUTING.zh-CN.md#新增一个-adapter)。评分公式见
[scoring.zh-CN.md](scoring.zh-CN.md)。

## 流水线

```
会话文件
     │
     ▼
adapters.Detect ──▶ adapters.Parse ──────▶ *trace.Trace
   (claude | kiro | kiro-cli | cursor | codex)  │
                                               ▼
                                 ┌─── analyzer.Analyze ───┐
                                 │                        │
                       context.Compute            mcp.Analyze
                       duplicate.Analyze          cache.Analyze
                                 │                        │
                                 └──▶ rules.Evaluate ─────┘
                                      score.Compute
                                      pricing（花费）
                                               │
                                               ▼
                                        analyzer.Result
                                               │
                        ┌──────────────────────┼──────────────────────┐
                        ▼                      ▼                      ▼
                 report/terminal          report/html            report/json
                                                                      │
                                                                      ▼
                                                                 store（历史）
```

store 挂在 JSON 渲染器上而不是与它并列：它写到磁盘上的东西**就是**那份 JSON 报告，所
以重新读取一个存下来的分析，得到的 `analyzer.Result` 和任何其他使用方拿到的完全一样。
见[持久化分析结果](#持久化分析结果)。

多个会话时，同一条流水线按会话各跑一遍，然后把**结果**折叠在一起：

```
[]analyzer.Result ──▶ overview.Build ──▶ *overview.Report
                                              │
                            ┌─────────────────┼─────────────────┐
                            ▼                 ▼                 ▼
                  terminal.WriteOverview  html.RenderOverview  JSON
```

数据单向流动。一个分析器读取 trace 并写自己的结果结构体；它绝不改动 trace，也绝不调用
另一个分析器。当一个分析器的输出要喂给另一个时——MCP schema 估算值会细化上下文归因的残
差——它是作为参数显式传进去的（`context.Compute(tr, split)`），而不是通过一个共享的可
变对象被发现的。

## trace 模型

`internal/trace` 是「某个运行时恰好如何写文件」与「AIProbe 知道什么」之间的边界。它之
上的一切都与运行时无关；像 `claude` 或 `kiro` 这样的包名不会出现在任何分析器里。

一个 `Trace` 是同一个会话的四个并行视图：

| 字段 | 是什么 |
| --- | --- |
| `Events` | 归一化后的时间线。每条消息、工具调用、工具结果和系统记录，按顺序排列，各自带一个估算 token 成本和一个类别。 |
| `Requests` | 每次 provider 调用一条，携带用量计数器和重建出的 prompt 前缀签名。 |
| `ToolUses` / `ToolResults` | 工具调用及其载荷，与 `Events` 交叉索引。 |

再加上会话自身的元数据、无法解码的行数，以及通过「哪些字段被填充了」间接表达的运行时能
力画像。

### 承载了大部分设计的三个字段

**`Event.Transient`** 标记那些只进入了恰好一个 prompt、随后就被运行时丢弃、而不是一直
往后携带的内容。这看起来像细节，其实不是：它会让一段上下文的成本相差「它之后还有多少次
请求」这么多倍。Claude Code 的转录是只追加的，所以没有 transient 内容；Kiro 会在产生
文件写入的那次调用之后丢掉写入体。在这个字段存在之前，把这些写入体计到之后的每次请求
上，让一个真实会话被高估了约 3 倍。

**`trace.Amount`** 把一个 token 数量与产生它的 `Method` 配对，并由此推导出一个四级
`Grade`：

| Grade | Method | 含义 |
| --- | --- | --- |
| `exact` | `provider-usage` | 逐字读自 provider 用量计数器。 |
| `estimated` | `heuristic-tokenizer`、`calibrated-estimate` | 由离线分词器计数，可能经过校准。 |
| `inferred` | `residual`、`scaled-context-usage` | 推导得出：实测总量减去一个估算值，或者一个上报的百分比乘以一个假定的窗口。 |
| `unknown` | `unknown` | 无法确定。 |

Grade 是从 method **推导**出来的，而不是与它并列存储，所以两者不可能互相矛盾。报告永远
不需要猜自己该对一个数字有多少信心：数字自己会说。

**`trace.OptInt` / `OptFloat`** 是可以合法地为「未知」的整数与浮点数，它们序列化成
`null` 而不是 `0`。正是这个机制阻止了「未知」在分析器与 JSON 之间的某处悄悄变成一个看
起来合理的零。如果你发现自己在为「柱状图宽度」之外的任何东西用 `.Or(0)` 解包它，那通常
是个 bug。

## Adapter

`adapters.Detect` 在可能的情况下按结构而不是按文件名识别运行时。这五种形状互不相容：

| 运行时 | 形状 |
| --- | --- |
| Kiro（IDE） | `{id, timestamp, payload}` |
| Kiro CLI | `{version, kind, data}` —— 没有 `payload`，也没有顶层 `type` |
| Codex CLI | `{timestamp, type, payload}`，并且 payload *里面* 还有第二个 type |
| Claude Code | 记录字段写在顶层，只有一个 `type` |
| Cursor | 根本不是 JSONL：一个带 `cursorDiskKV` 表的 SQLite 数据库 |

所以检测不依赖某个未来版本可能会改的路径约定。Claude Code 是兜底项，因此其他每个运行时
都必须排在它前面探测；一个忘了把自己加进来的 adapter 会被静默当成 Claude Code 解析，产出
一份看起来合理但错误的报告 —— 这就是 `TestDetectRoutesEachRuntimeToItsOwnParser` 要覆盖
每个 kind 的原因。

Cursor 是「一个文件一个会话」的例外：它把所有对话都放在同一个数据库里，所以一个会话要用
`state.vscdb#<composerId>` 来定位。只给一个裸数据库路径时，它会选其中最大的那段对话并明
说自己这么做了，而不是把互不相关的对话混成一条根本没发生过的 trace。

每个 adapter 还会公布一份 `Capabilities` 画像，报告靠它决定某个区块是有意义的、还是应
该说「这个运行时没说」：

这张表按运行时排行、而不是按能力排行，因为真正有意思的读法是横着读一行：这个运行时会告
诉你什么、不会告诉你什么。

| 运行时 | token 用量 | 缓存读取 | 缓存写入 | 系统提示 | MCP 来源 | 上下文窗口 |
| --- | --- | --- | --- | --- | --- | --- |
| Claude Code | 有 | 有 | 有 | 无 | 无 | 无 |
| Kiro（IDE） | 无，按 credit 计费 | 无 | 无 | 有 | 有 | 百分比形式 |
| Kiro CLI | 无，按 credit 计费 | 无 | 无 | 无 | 无 | 百分比**并且**明确给出尺寸 |
| Cursor | 无，按 request 计费 | 无 | 无 | 无 | 无 | 无 |
| Codex CLI | 有 | 有 | 无 | 有 | 无 | 尺寸形式 |

前四个运行时几乎是互补的，而这正是有用的地方。Claude Code 给出精确 token 但隐藏系统提
示，所以 `系统提示 / 规则` 只能是一个残差。Kiro 存了系统提示但不给 token，所以一切都是
估算，花费也只能靠推导。Codex 上报 token 和窗口，但无法区分一个 MCP 工具和一个内置工
具，所以 MCP 区块可能少算。

有两行值得细读。

**Kiro CLI 不是 Kiro。** 同一个厂商，磁盘格式毫无关系，能力画像也不同，所以它是一个独立
的 adapter 而不是一个分支。它的按轮次记录里*确实有* `input_token_count` 和
`output_token_count` 字段，而在实测会话里它们是 `0`，同时同一轮还上报了真实的 credit 扣
费和真实的利用率 —— 一轮被测为「已用 200,000 token 窗口的 2.2753%」，也就是约 4,550
token，却上报成零。所以这项能力写「无」：宣称有精确 token 记账、然后打印一堆零，正是这
个项目存在的意义所要避免的失败。适配器仍然会读这两个字段，并在它们非零时把它们提升为实
测用量。它比 IDE 版多出来的东西是**明确给出的**窗口，所以利用率是对着一个上报值换算，而
不是对着一个假设值。

**Cursor 什么数字都不给。** 没有 token、没有缓存、没有花费、没有窗口、没有利用率。它是唯
一一个没有任何东西可以锚定或交叉校验估算的运行时，所以一份 Cursor 报告里的每个数字都是
AIProbe 自己算的，而报告会用一条警告说明这件事，而不是塞在脚注里。完好保留下来的是内容
层面的分析：重复上下文、被读了两次的文件、过大的工具结果、臃肿的工具 schema —— 这些是关
于「对话里有什么」的结论，不是关于「账单是多少」的结论，它们不需要一个 provider 总数才能
成立。

trace 模型必须让这五者都诚实，这就是为什么能力是数据，而不是散落在各个分析器里的
`if adapter == "kiro"`。

## token 估算，以及如何让它保持诚实

`internal/tokenize` 是一个字符类启发式算法，不是真正的分词器：没有词表文件、没有模型下
载、不联网。它刻意是近似的，它产出的一切都被标为 `estimated`。

有两个机制阻止这种近似漂移成虚构。

**校准。** 当一个会话有连续的、带实测用量的请求时，两者之间实测 prompt token 的增长，
会与估算器对「这期间新增内容」的计数相比较。取所有这类配对的比值中位数，用它缩放后续的
估算。demo 会话报的是 `×1.00`，因为不需要校正。校准至少需要两个样本；低于这个数就用原
始值，并有一条说明写明这一点。

**交叉校验。** 没有 token 计数器的运行时无法被校准，所以必须有别的东西来伪证这个估算。
两个 Kiro 变体都会按轮次上报上下文窗口利用率，`kiro.Check` 就拿估算值去对照那条曲线。
CLI 适配器复用同一个函数而不是抄一份，这样两者不会各自漂移；不同的是这个答案*意味着*什
么，而 CLI 那一侧更强，因为它明确给出了窗口，而不是留给别人去推断。

Cursor 连利用率都不上报，所以它既没有校准也没有交叉校验。这一点会写在报告里，而不是被糊
过去。

Kiro CLI 的这组配对还让一件事变得可以证明。CLI 不保存系统提示，也不保存工具 schema，所
以拆解里没有 `系统提示 / 规则` 的内容，否则就会暗示「每次请求的固定开销是零」。它不是
零，而且这次难得是算术而不是论证：运行时同时给出了窗口有多满、以及窗口有多大，那个乘积
与转录文件能解释的一切之间的差额，就是被发送但没有被保存的内容。适配器会连着数字一起把
这个差额报出来。它不会把它变成拆解表里的一行，因为要在系统提示和工具 schema 之间分账，
这个文件并不支持。

微妙之处在于：一个百分比只有在你知道窗口大小时才能变成 token 数，而 Kiro 从不说窗口是
多少。用数据去拟合窗口只会强行造出一致性，什么也证明不了。所以这个校验改为报告两个与窗
口无关的统计量：

- **跟踪度（Tracking）** —— 各样本推算出的窗口的 p90/p10 离散度。接近 1.0 意味着估算值
  与运行时自己的记账成正比，无论窗口到底是多少。
- **推算窗口** —— 这个配对隐含的窗口大小。它是校验的**输出**，绝不是输入。

只有当形状和尺度都吻合时，归因才被称为可信。仅有形状不够：一个跟踪得完美但系统性偏大 2
倍的估算，仍然会给出错误的分类别 token 数。

## 分析器

每个都是独立的，每个都拒绝猜。

**`context`** 把每一个计费的 prompt token 归到七个类别之一，而且是**累计**的：内容按
「有多少次请求必须携带它」计入，这就是为什么一个 40 轮会话里第 3 轮读的 6k 文件成本是约
22 万而不是 6 千。`系统提示 / 规则` 是实测总量减去估算的可见上下文之后剩下的部分——一个
残差，并且被标为 `inferred`，而不是被当作实测值呈现。

**`mcp`** 用一份可选的本地清单为工具 schema 定价。工具 schema 从不会被写进转录文件，所
以没有清单时，加载的工具数量、利用率和 schema 成本全都是 `unknown`。分母不会被凭空造
出来。

**`duplicate`** 用归一化内容指纹找完全重复，用超过阈值的行级 shingle Jaccard 相似度找
近似重复。纯词法：没有 embedding、不调用模型，所以它在毫秒级完成，而且每次给出相同答
案。它把插入浪费（一个副本一次性的成本）和计费浪费（这个成本乘上携带它的请求次数）分开
报告，因为只有第二个才值得据以行动，也只有第一个才是可回收的内容。

**`cache`** 逐字采用缓存计数器，然后解释其中的缺口。对每个漂移事件，它比较相邻请求重建
出的 prompt 前缀并归因出一个原因——压缩、模型切换、TTL 过期、前缀重写、规则变更、元数据
变更、工具集变更、易变内容——每个都带一个置信度，或者在转录文件无法解释时给 `unknown`。
在这里猜是主动有害的：空闲超时的修法和规则文件被编辑的修法毫无共同之处。

**`rules`** 在其他分析器的输出上跑 17 条确定性规则。每条都返回它据以触发的证据、可回收
的 token、要做的改动，以及一个置信度。没有模型参与。

**`score`** 是一个公开的五子项公式，无法评估的子项报为未知，其权重被重新分配而不是记为
零分。见 [scoring.zh-CN.md](scoring.zh-CN.md)。

## 把 token 换算成钱

`internal/pricing` 持有价目表；`analyzer.Spend` 持有结果。有四个决策承担了主要重量，其
中后两个之所以存在，是因为「显而易见的实现」错了，而且是靠看真实输出才发现的。面向用户
的细节在 [pricing.zh-CN.md](pricing.zh-CN.md)。

**每个模型四种费率，而不是一种。** `pricing.Price` 是
`{Input, Output, CacheWrite, CacheRead}`。Anthropic 的缓存写入按输入价 1.25 倍、读取按
0.1 倍计费，所以一个混合的「每 token 单价」在本工具存在的意义所在的那类会话上，会差一
个数量级。这个四路拆分也正是让花费区块具备诊断价值而不只是装饰的原因：一行很大的缓存读
取是便宜的，一行很大的输入不是，而单看 token 数说不出你遇到的是哪一种。

**规则引擎对钱一无所知。** `rules.Evaluate` 保持为其他分析器输出的纯函数。花费是分析器
在之后附加上去的，填进 `Recommendation.WastedCost`，这样一条规则不可能变成依赖某个价
格，而且无论定价开着还是关着，推荐结果都逐字节相同。`Options.Pricing` 为 nil 时压根不
产生 `Spend` 区块，而不是产生一个填满零的区块，这样使用方能区分「定价被关掉了」和「这
个不花钱」。

**请求逐个定价，然后按模型分组。** 一个从 Opus 切到 Haiku 的会话，会按各自模型的费率计
费。早期版本把整个会话归到它见过的最贵的模型上，结果仅凭一次无关紧要的请求，就把 demo
会话按 $15/M 重新标价了。

**可回收的浪费按会话自身的有效 prompt 单价计价**，计算为
`(输入$ + 缓存写入$ + 缓存读取$) / prompt token 数`，而不是按标价的输入费率。大部分可
回收的上下文本来就是可缓存的——工具 schema 处在缓存前缀里，一个被读三次的文件有两次来自
缓存——所以会话为携带它支付的只是输入价的一小部分。按完整价计价，会算出「账单 $1.65、却
声称能省 $5.95」：不只是不精确，而是不可能，也是让读者对其他每个数字都失去信任的最快方
式。有一个测试断言可回收永远不能超过已花掉的钱，而每条发现的花费用的是同一个单价，所以
它们加起来不可能超过总额。

还有两条较小的规则，遵循的是这个代码库其余部分同样的原则：一个说明清楚的「未知」胜过一
个编造出来的数字。

- **未列出的模型会被近似，而不是被隐去——但绝不会被似是而非地猜。** 它按一个通用兜底值
  定价，结果带上 `SourceFallback`，每个渲染器都会把它变成一条点名了模型的带标注的告
  警。最初的设计把它报为未知；那个判断对**没有标注的**数字是对的，在这里错了，因为任何
  使用比表更新的模型的人会完全看不到花费。仍然被拒绝的是那种**看起来像知识**的猜测：前
  缀匹配会解析一份修饰词白名单（`-latest`、`-thinking`），但绝不解析版本号跃升，所以
  `claude-opus-4-7` 不会继承 `claude-opus-4` 的价格。这两代价格差三倍，而且这个问题是
  在真实使用中发现的，它把一个真实会话报错了 3 倍。兜底值可配置，也可以关掉。
- **没有计数器的运行时会得到一个推导出的花费，而这花了两次尝试。** Kiro 不上报 token 用
  量，并按 credit 计费。第一个版本完全不给它任何金额，这让这个功能对最有可能被指向本工
  具的那个运行时彻底不可见。第二个版本把它全部累计上下文按未缓存输入价定价并称之为上
  限——技术上没错，实用上没用，因为它读出来是**$5,367，而这个会话推导出来是 $547**。

  真正的修法是意识到：缓存划分**是可以推导的**。上下文归因里的 `Inserted` 是内容的一次
  性成本、不计重发；内容写入缓存一次，之后每次请求都从缓存读回，所以 inserted 就是缓存
  写入那一侧，累计总量剩下的部分就是读取。在一个真实会话上这是 99.8% 读取，这也正是那
  个未缓存读数偏差十倍的原因。

  `modelledUsage` 从这两个数字合成出一个 `trace.Usage`，交给实测会话走的同一条定价路
  径，所以分类别拆解、可省回数字和每条发现的花费全都无需特例即可工作。它**没有**建模的
  是缓存过期，这让那个数字成为下限；`Spend.Ceiling` 把全未缓存的读数保留为对应的天花
  板，两个都会展示。运行时自己的 credit 总量一并列出，两者互不换算。

花费继承它所依据的 token 的来源属性。`pricing.Cost.Estimated` 由调用方自己的标记设置，
渲染器给估算花费加上的前导 `~` 与它给估算 token 数加的完全一样。可省回数字永远是估算
的，因为它背后的浪费是分析结果而不是计费数据——即使会话自身的花费来自 provider 计数器也
是如此。

## 跨会话汇总

`internal/analyzer/overview` 把多个 `analyzer.Result` 折叠成一份报告。关键决策是：它汇
总的是**结果**，不是会话。

把每份转录文件拼成一个 trace 再跑常规分析器，代码会更少，也会以一种很难被注意到的方式
出错。有两个分析器只在一次对话内部才有意义：

- **重复检测**会把在会话 A 读过、又在会话 B 读过的文件当成冗余。它不是：每个会话都需要
  自己的那一份，因为它们是不同的上下文。这样汇总出来的浪费，是你怎么改变工作方式都拿不
  回来的。
- **缓存归因**比较相邻请求的 prompt 前缀。跨越会话边界时，前缀理应不同，所以每个边界都
  会表现为一次找不出原因的缓存未命中。

所以每个会话都完全按它单独分析时的方式被分析，汇总只是对输出做求和与排序。因此汇总里的
每个数字，含义都和单会话报告里完全一样，这也正是两者可比的原因。

汇总本身引入了一个新的诚实性问题。把一个「在某些会话里未知」的量求和，得到的是一个**下
限，不是总量**——一个 MCP 成本无法测量的会话贡献了零，而一个天真的总量读起来就像那个会
话是干净的。所以 `overview.Sum` 携带它无法测量的会话数量，`Complete()` 报告是否有缺
失，两个渲染器都会给不完整的求和加上标记（终端里是 `≥`，HTML 里是「至少」），而不是把
它当作完整值呈现。

出于同样的原因，评分是以分布形式报告的——中位数、最好、最差、等级直方图——而不是被平均成
一个看起来像某个会话成绩的单一数字。

金额通过 `overview.MoneySum` 得到同样的处理，它就是货币版的 `Sum`：携带它无法定价的会
话数量，渲染器以同样方式标记不完整的总额。它还多一条自己的规则——一组会话如果用了不同的
货币标签，就**不会**被相加，因为货币之间的换算需要一个本工具没有诚实途径获取的汇率，所
以币种不同的会话被计为未定价，报告也会说明。

汇总是本地服务端唯一会缓存的东西，因为它是唯一一个要解析每个会话而不是一个会话的东西。
缓存键是运行时加上会话集合的指纹，并且刻意**不**按语言做键：分析器记录的是消息 id 而不
是句子，所以一份汇总可以用任何语言渲染，切换语言不花任何成本。

## 持久化分析结果

`internal/store` 把每次分析都存在磁盘上。有两个原因，而第二个才是真正重要的那个：

1. 没有它，本地服务端每次页面加载都要重新解析、重新分析。打开一份报告、切换语言、按返
   回，就是对同一批未变的字节做三次完整分析——真实会话上每次约 190 毫秒，从 store 读只要
   8 毫秒。
2. 压根就不存在「分析历史」这回事，所以「我的上下文使用在变好吗」没有答案，尽管每次运行
   其实都已经把它算出来了。

### 缓存键必须包含构建版本

`store.Fingerprint` 对会话路径、大小、mtime 以及**工具版本**做哈希。版本正是让升级后重
新分析、而不是重播旧构建结论的东西；把它去掉会让每一次改进在已经进过历史的会话上都不可
见。

但这只在版本真的会变时才成立。它没有变：树里没有 git tag，`git describe` 失败，于是每
个构建都自称 `0.1.0-dev`，旧构建存下的分析就精确地命中了指纹。这个症状传到了用户那里
——一份报告持续渲染「花费未知」，而当前的二进制其实把这个会话定价为 $10.64，只因为服务
出去的页面来自缓存。现在 `make build` 会给任何非发布版本追加一个构建戳，而
`buildVersion` 会为 `go install` 回退到 Go 内嵌的 VCS 戳。

`Server.usable` 是这条保险带之外的另一道：它会拒绝那种「spend 区块以当前构建绝不会产生
的方式为空」的存档分析。它之所以存在，是因为版本方案是一种约定，而约定会被破坏，也因为
这个失效模式是静默的。

### 存下来的制品就是那份报告

```
~/.aiprobe/
├── index.json            每次分析一条记录：身份、时间、几个头部数字
└── analyses/<id>.json     完整报告，与 `--json` 逐字节一致
```

`index.json` 重复保存了头部数字，这样列出历史不必打开几百个文件。分析文件本身走
`report.WriteJSON`，也就是 `--json` 用的同一个函数，所以不存在第二种需要同步维护的序列
化格式，也不存在一份「读得懂 JSON 报告的人却解析不了」的存档文档。

### 身份：什么算缓存命中，什么算一条趋势

两个键，做不同的事：

| 键 | 由什么组成 | 职责 |
| --- | --- | --- |
| `Fingerprint` | 会话路径、大小、mtime、**工具版本** | 这次分析是不是已经做过了 |
| `SessionKey` | adapter、会话路径 | 哪些分析属于同一个会话 |

工具版本放进指纹是有意的：由旧构建产出的分析，与本构建的不可比，所以必须重跑而不是直接
服务出去。`Entry.Stale` 让历史页面可以说明这一点，而不是把两者画在同一条线上。

由于指纹包含大小和 mtime，重新分析一个**未变化**的会话会替换它原有的条目，而一个已经增
长的会话会新增一条。正是这种不对称在不堆积重复行的前提下产生了趋势：历史页面每个会话显
示一行，而重复的分析变成了那一行上的迷你折线图。

### 为什么用文件而不是 SQLite

需要的查询是：按指纹取一条分析、按时间倒序列出、按会话过滤。三个查询，几百条记录。没有
一个需要 SQL。

真正定下这个决定的是替代方案的实测代价。纯 Go 驱动 `modernc.org/sqlite` 会把模块依赖图
从 1 个依赖变成 26 个，其中包含一份 libc 的 C 到 Go 翻译；而 cgo 驱动则会破坏发布构建所
依赖的纯 Go 交叉编译。对于一个读别人 prompt、并且主动要求被审计的工具来说，为了省掉一百
行文件处理代码，而大幅增加审阅者必须信任的东西，是不划算的。

文件还保住了一个在这里比查询灵活性更值钱的属性：这份历史可以用文本编辑器读和删，检查它
不需要任何工具。`internal/store` 是一个包、一个接口，所以万一真的出现了文件确实服务不了
的查询，那些报告仍然是可供迁移的事实来源。

### 不用 SQLite 驱动来读 SQLite

上面那个决定仍然成立，然后 Cursor 从另一头把这个问题又提了一遍。Cursor 只把聊天历史存在
一个 SQLite 数据库里，所以要支持它，要么终究还是把依赖加进来，要么直接去读文件格式。

`internal/sqlitedb` 选择读字节。它刻意不是一个 SQLite 实现：没有 SQL、没有查询规划器、没
有写路径、没有索引、没有事务。整个对外能力就是「遍历一张表的行」，而这就是一个 adapter
需要的全部。它确实实现了的，是这个格式为了读真实文件所必须的部分 —— 文件头、
`sqlite_master` 目录、表 b-tree 的内部页与叶子页遍历、带全部 serial type 的记录格式、
溢出页链，以及预写日志恢复的读取那一半。溢出页不是可选项：一条聊天消息经常比一页还大，
忽略它就会静默截断恰好最值得读的那些行。

预写日志同样不是可选项，这一点是一份 bug 报告逼出来的。处于 WAL 模式的数据库会把最近提交
的页留在 `-wal` 边车文件里，直到某次 checkpoint 把它们折回主文件；而 Cursor 只要编辑器在
运行就一直持有它的存储，所以全局库永久处于 WAL 模式，最新的对话有一部分就住在那个文件
里。拒绝读 WAL 数据库 —— 也就是最初的行为 —— 是诚实的，但它覆盖了每一个用过的 Cursor 安
装：`aiprobe runtimes` 在有几百个会话的机器上报告「no sessions」。所以边车文件会被扫描并
恢复，方式和 SQLite 在没有其他连接建好共享内存索引时一样：用 salt、累积的斐波那契加权校
验和链、以及提交标记来判定哪些帧是有效的，得到的页覆盖主文件中的同号页。某一帧的校验和
断了链，就结束扫描而不是让整次读取失败，这样一个边读边写的写入方留下的只是一个被撕裂的
尾巴，而不是一个错误。

有两件事让整个做法是站得住脚的，而不是莽撞的。

**它宁可拒绝也不猜。** 一个存在但无法解释的 `-wal` 边车文件是一个带解释的错误，而不是可
以忽略的东西：忽略它就等于把主文件当成完整的来上报。任何这个格式认为不可能的情况 —— 单
元格指针指到页外、b-tree 成环、记录被截断、本该是表的地方出现索引页 —— 同样是错误。一个
跳过读不了的页的读取器，会上报一个更小的会话，而不是一次失败，那才是更坏的结果。边车文
件干脆不存在时，主文件就按原样读，这也是 SQLite 自己的做法：一次干净的关闭会把日志
checkpoint 回主文件并删掉它。

**它是对着 SQLite 自己的输出验证的。** `internal/sqlitedb/testdata` 下的固定样本是用
`sqlite3` 命令行写出来的，不是这个包自己写的，因为一个只对着自己输出做校验的解析器什么也
证明不了。这些样本包括：一个跨约五十个溢出页的 26,800 字节值，用摘要逐字节比对；一张在
512 字节页上的 400 行表，让 b-tree 的叶子之上真的有内部层级，同时还夹着读取器必须拒绝的
索引页；以及这个记录格式定义的每一种 serial type。

store 仍然用文件。读别人的数据库这件事，并不改变这个工具自己该写什么。

### 失败行为

- 写入走临时文件、`fsync`、`chmod 0600`、再重命名，所以被中断的运行不可能留下半写的索引
  或不完整的报告。
- 损坏的 `index.json` 会退化为一份空历史，而不是报错。历史是便利功能；因此拒绝分析会是
  对真正职责的倒退。
- 历史有上限（默认 500 条），淘汰时会连分析文件一起删掉，所以一个本地诊断工具不可能在别
  人的 home 目录里无界增长。
- 内置的 demo 从不存储，理由与汇总排除它相同：让合成数字进入真实趋势，就是把虚构当成测
  量。

## 从一条发现到一件可做的事

`internal/analyzer/rules` 负责检测。`internal/analyzer/rules/advice.go` 负责说该拿它怎
么办。把这两件事分开，比看起来更重要。

建议是**按规则 id** 解析出来的，而不是写在规则体里。`Evaluate` 推导出
`rule.RNNN.step.1`、`.step.2` 等等，一直数到某个目录条目缺失为止。两个后果：

- 想让一条规则更可执行，只需要加目录条目。检测逻辑一行不改，所以改进建议不可能弄坏某个
  阈值。
- 步骤数量是从**英文**目录推导的，所以一份缺了某条的翻译会把那一步显示为英文，而不是在
  某种语言下静默地给出一个更短的列表。

工具提示走同样的路子，身份是数据、文案是消息 id：

```go
"rtk": {
    Name:      "rtk",
    Install:   "cargo install rtk  # …",
    DocRef:    PlaybookRef + "#rtk",
    WhatMsg:   Msg{ID: "tool.rtk.what"},
    VerifyMsg: Msg{ID: "tool.rtk.verify"},
},
```

有三条规则约束里面能放什么。

**每条提示都带一个验证方法。** 推荐一个安装命令却不给出「怎么确认它起作用了」，正是一份
工具清单退化成链接农场的方式。每个 `VerifyMsg` 都点名一项本工具会做的测量，这样闭环就
成立了：分析、改一件事、再分析。

**不放 URL。** `DocRef` 是一个仓库内的相对路径。HTML 报告是自包含的，而
`TestHTMLReportIsSelfContained` 会在渲染出的字节里搜 `https://` 且没有白名单，所以链接
只住在 playbook 里，报告指向 playbook。接着 `TestPlaybookReferencesResolve` 会检查每个
路径都落在一个真实存在的标题上——一份指向失效锚点的报告，比一份什么都不指的更糟，而且别
的东西都不会注意到。

**大多数规则没有工具。** 十七条里有九条有。MCP 配置、模型选择和缓存卫生都是要改的设置；
给它们全都硬塞一个工具，只会让那些真正有意义的贬值。

跨会话汇总通过 `StepsByRule` / `ToolsByRule` 从规则 id 解析建议，而不是自己带一份副本，
所以 `overview.FindingTotal` 及其 JSON 形状都没有变化。

## 报告与本地化

三个渲染器消费的是同一个 `analyzer.Result`。JSON 输出是契约；终端与 HTML 视图是它的呈
现形式。

渲染器是**唯一**把数据变成句子的层。分析器在任何人知道报告将用什么语言之前就已经跑完了
——`aiprobe serve` 会用一次运行的结果渲染两种语言——所以一个想说点什么的分析器记录的是一
个 `i18n.Msg`：一个消息 id 加它的参数。由报告器在目标语言下渲染它。

这还有第二个好处。JSON 报告携带的英文字符串，与翻译版本渲染自同一个目录条目，所以两者不
可能描述不同的东西。任何分析器包里都没有英文散文。

两个值得知道的后果：

- 占位符是命名的（`{tokens}`），不是位置的。中文和英文的从句顺序不同，所以译者必须能把
  一个值挪到句子需要它的位置。
- 一个内嵌了可翻译术语的句子——比如某条规则证据行里嵌着一个缓存漂移原因——会把它作为
  `i18n.Ref` 或嵌套的 `i18n.Msg` 传递，在渲染时才解析。在分析器里把渲染好的术语拼接起
  来，会把语序钉死在英文上。

终端渲染器的每一列宽度都由配置的总宽度推导，并且按显示单元格而不是 rune 来度量字符串，
因为 CJK 字形是双倍宽。有一个测试断言在两种语言、多个宽度下都没有行溢出。

HTML 渲染器输出单个自包含文件：内联 CSS、内联 JavaScript、由 Go 生成的 SVG 图表。没有
CDN、没有 web 字体、没有分析脚本。CI 会在产物里搜外部 URL，而不是听信这个说法。

## 被强制执行、而不只是「打算做到」的事

| 属性 | 由什么强制 |
| --- | --- |
| 二进制里没有网络客户端 | `TestNoForbiddenImports` 走一遍导入图；`internal/serve` 是唯一例外 |
| `serve` 只绑定回环地址 | `TestNonLoopbackHostIsRefused` —— 包括「无法解析的主机名被拒绝而不是被假定安全」 |
| 服务出去的页面什么都不去取 | `TestServedPagesReferenceNoExternalOrigin`、`TestHistoryReferencesNoExternalOrigin` 直接搜渲染出的字节 |
| 发现里不含 URL，只有仓库路径 | `TestToolHintsCarryNoURLs` |
| 节省额永远不会大于它所来自的花费 | `TestRecoverableNeverExceedsWhatWasSpent`、`TestFindingCostsAreConsistentWithTheTotal` |
| 浪费按缓存实际的花费定价，而不是按标价输入 | `TestEffectiveRateReflectsCaching` |
| 近似出来的花费会被标注，版本跃升不继承价格 | `TestUnlistedModelIsApproximatedAndSaysSo`、`TestVersionBumpsAreNotPricedByPrefix` |
| 配一个模型只需两个数，显式的零会被保留 | `TestConfigNeedsOnlyInputAndOutput` |
| 通用单价可以关掉，恢复明确的「未知」 | `TestFallbackCanBeSwitchedOff` |
| 没有计数器的运行时得到一个标为估算、且在自身上限之下的推导值 | `TestNoCountersProducesAModelledCost`、`TestModelledSessionCarriesItsCeiling` |
| 推导值应用缓存划分，而不是未缓存费率 | `TestModelledCostIsNotTheUncachedReading` |
| credit 总量绝不会被当作美元总量 | `TestCreditBilledRuntimeKeepsItsCreditsAndGetsAFigure` |
| 有实测数据的会话不会多长出一个「最多」数字 | `TestMeasuredSessionHasNoCeiling` |
| 单价在本地服务端的每个页面都能到达 | `TestPricingIsReachableFromEveryPage` |
| 来自另一个构建的缓存分析绝不会被服务出去 | `TestEmptySpendFromTheSameVersionIsReanalysed`、`TestVersionIsSpecificEnoughToInvalidateACache` |
| `pricing --init` 不可能毁掉你手填的费率 | `TestWriteRefusesToClobber` |
| 报告里每个 playbook 引用都落在真实标题上 | `TestPlaybookReferencesResolve` |
| 每条规则在每种语言下都有可执行步骤 | `TestEveryRuleHasExecutableSteps`、`TestStepsExistInEveryLocale` |
| 每条工具提示都说明怎么验证它起了作用 | `TestEveryToolHintCanBeVerified` |
| 两份样式表定义同一套浅色调色板 | `TestThemesAgreeAcrossStylesheets` |
| 每个页面都提供主题切换，并在首次绘制前生效 | `TestEveryPageOffersTheThemeToggle` |
| 新 adapter 被接进了每一处，而不只是 `Kinds` | `TestEverySelectableKindIsFullyWired`、`TestDetectRoutesEachRuntimeToItsOwnParser` |
| 不报 token 的运行时必须说清自己按什么计费 | `TestEverySelectableKindIsFullyWired` |
| SQLite 读取器与 SQLite 自己写出的内容一致，含溢出页 | `TestOverflowChainIsFollowedExactly`、`TestInteriorPagesAreTraversed`、`TestEverySerialTypeDecodes` |
| WAL 数据库通过它的日志被读到，而不是读成过期数据 | `TestWALSidecarRowsAreRead`、`TestWithoutItsLogTheSameFileIsStale`、`TestConversationsLivingOnlyInTheWriteAheadLogAreFound` |
| 无法解释的日志被拒绝，而不是被忽略 | `TestUnreadableSidecarIsRefused` |
| 边读边写留下的是被撕裂的尾巴，而不是一个错误 | `TestPartialTailFrameIsIgnored`、`TestStaleFramesAfterACheckpointAreIgnored` |
| 损坏表现为错误，而不是一个更短的会话 | `TestCorruptionIsAnErrorNotASilentShortRead` |
| 双文件会话不会被自己的边车文件重复计数 | `TestSidecarAssistantCopyIsNotCountedTwice` |
| 存在但为零的 token 计数器不会被当成实测值 | `TestZeroTokenCountersAreNotTreatedAsMeasured` |
| 一个装了多段对话的数据库不会静默把它们混在一起 | `TestBareDatabasePicksTheLargestAndSaysSo`、`TestNamedConversationIsSelectedExactly` |
| 读不懂的工具记录会被计数，绝不被编造成一个无名工具 | `TestUnreadableToolRecordIsCountedNotInvented`、`TestUnreadableToolRecordDoesNotInventATool` |
| 路径在每个 adapter、每个平台上都按同一套规则比较 | `TestSameDirIgnoresSpelling`、`TestSameDirCaseFollowsThePlatform` |
| 测试用当前平台自己的环境变量重定向 home 目录 | `TestHomeEnvVarMatchesWhatUserHomeDirReads` |
| 颜色只判断一次，并且绝不写进管道 | `TestColorEnabledRespectsEveryOptOut`、`TestSessionListIsPlainWhenNotATerminal` |
| 报告永远不是死路 | `TestReportOffersAWayBackToTheSessionList` —— 它必须提供返回路径，且不得自称是一个导航目的地 |
| 跑测试不会动到你的历史 | `TestAnalysesAreNotStoredDuringTests`、`TestDemoIsNotRemembered`（内含一个阳性对照，所以它不可能因为「存储坏了」而通过） |
| 根目录的 main.go 保持只有转发 | `TestRootMainStaysTrivial` |
| 公开的评分文档与代码一致 | `TestDocMatchesImplementation` |
| 评分可复现 | `TestDeterministic` |
| README 与 `docs/demo-report.txt` 引用的是真实输出 | `TestReadmeHeadlineMatchesDemo`、`TestTranslatedReadmesStayInStep`、`TestCommittedDemoReportIsCurrent` |
| 用到的每个消息 id 在每种语言下都真实存在 | `TestLiteralMessageIDsExist` |
| 各语言覆盖相同的 id、且占位符相同 | `TestEveryLocaleCoversEnglish`、`TestNoLocaleInventsIDs`、`TestPlaceholdersMatch` |
| 交叉校验不依赖假定的窗口大小 | `TestWindowIndependence` |
| `aiprobe demo` 在五秒内完成 | `TestDemoIsFast` |

如果一个改动触发了上面任何一条，正确的修法几乎从来不是放宽那个测试。
