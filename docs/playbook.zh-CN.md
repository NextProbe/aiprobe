# 省 token 实战手册

*[English](playbook.md)*

AIProbe 告诉你一个 agent 会话的 token 花在了哪里。这份文档是另一半：别人已经造了哪些
东西来把这些 token 拿回来、每种做法真正省的是什么，以及怎么在**你自己的**负载上验证
它有没有用，而不是在别人的 benchmark 上。

报告里的发现会链接到这里。如果你是从 `docs/playbook.md#某个锚点` 跳过来的，那一节就是
你要看的内容。

## 先读这一段

**测量 → 只改一件事 → 再测量。** 下面每种技术，网上都能找到一个数字。这些数字几乎都
不会是你的数字，因为它们是在别的负载上测的。唯一值得据此行动的数字，是从你自己的会话
里量出来的那个——而这正是 AIProbe 存在的理由：

```sh
aiprobe analyze                     # 改之前
# 只改一件事
aiprobe analyze                     # 改之后
aiprobe history                     # 两次之间的趋势
```

**「省 token」其实是四个互不相干的问题。** 它们经常被当成一个话题讨论，但它们不是。
混在一起谈，就会出现「装了一个 KV cache 工具来降低 Anthropic 账单」这种事。

| | 减少的是什么 | 在哪里生效 |
| --- | --- | --- |
| **上下文压缩** | 你发出去的 token | 请求之前 |
| **缓存** | 请求本身（干脆不发） | 请求之前 |
| **模型路由** | 每 token 的单价，不是 token 数量 | 分发时 |
| **KV cache 压缩** | 推理时的 GPU 显存 | 模型服务内部 |

如果你调的是托管 API，最后一项跟你完全无关，不管它的数字多好看。

**很少有人提的那个乘数。** 在 agent 会话里，你加进去的 token 不是只付一次。之后每一次
请求只要还携带它，就要再付一次。一个 40 轮的会话里，第 3 轮读进来的 5,000 token 文件，
实际暴露成本约 185,000 token，不是 5,000。这就是 AIProbe 为什么把「计费」和「插入」
分成两列，也是为什么**早一点少读一点，比晚一点少读很多更值钱**。

## 六层

```
                     agent / IDE / CLI
                            │
   ① 精确缓存 ───────────────┤   这个一模一样的问题我答过吗？
   ② 语义缓存 ───────────────┤   有没有答过足够接近的问题？
                            │
   ③ 工具输出压缩 ───────────┤   输出 5,000 行，有用的 20 行
   ④ prompt 压缩 ───────────┤   常驻的开场白太长了
   ⑤ 历史压缩 ──────────────┤   现在对话本身成了成本大头
                            │
   ⑥ 模型路由 ──────────────┤   这一轮不需要用大模型
                            ▼
                         provider
```

①② 是**不发这次请求**。③④⑤ 是**把请求变小**。⑥ 是**让它更便宜**。它们可以叠加，而且
大致就该按这个顺序做，因为「不发」永远比「发小一点」划算。

### 你的报告指向哪一层？

| AIProbe 发现 | 层 | 章节 |
| --- | --- | --- |
| R008 重复工具结果、R009 重复命令、R010 超大输出、R016 工具热点 | ③ | [工具输出压缩](#工具输出压缩) |
| R003 反复读同一批文件、R011 整份文件读取 | ③ | [先定位再读取](#先定位再读取) |
| R012 每次请求的固定开销 | ④ | [prompt-压缩](#prompt-压缩) |
| R013 反复压缩、R017 对话主导成本 | ⑤ | [历史压缩](#历史压缩) |
| R001 / R002 MCP 工具税 | — | 这是配置问题不是压缩问题：把用不到的服务器关掉 |
| R004 到 R007、R014 prompt 缓存 | — | 属于缓存**卫生**，见[让前缀保持稳定](#让前缀保持稳定) |

## 工具输出压缩

*第 ③ 层。* 对写代码的 agent 来说这一层收益最高，因为工具输出通常是报告里最大的一类，
而且是最容易在不丢信息的前提下砍掉的一类。

最值得记住的一句话，RTK 作者说得很直接：**确定性规则能做的事，就不要用模型做。**
999 个通过 1 个失败的测试结果，不需要一个语言模型来判断哪部分重要。

### jq

[jq](https://github.com/jqlang/jq) —— 你机器上几乎肯定已经装了。

最便宜的压缩是「剩下的部分根本不取」。一个工具返回五十个字段而下一步决策只需要两个，
这不是压缩问题，是查询问题：

```sh
gh pr view 42 --json title,state,files | jq '{title, state, files: [.files[].path]}'
kubectl get pods -o json | jq '.items[] | select(.status.phase != "Running") | .metadata.name'
```

**为什么它排在这一层最前面。** 这一层其他工具都是在数据已经产生**之后**再决定丢什么。
在源头选字段不可能丢掉模型需要的东西，因为需要什么是你自己定的。没有模型、没有需要调的
压缩比、没有质量风险。

**怎么验证。** 看「工具结果」这一行。这一行大幅下降而结论不变，是这份文档里最干净的一种
胜利。

### rtk

[github.com/rtk-ai/rtk](https://github.com/rtk-ai/rtk) —— MIT，单个 Rust 二进制，
不联网，无遥测。

一个 CLI 代理，夹在 agent 执行的 shell 命令和模型看到的输出之间。它对大约一百个常见
开发命令（git、测试、构建、包管理、容器工具）都有专门的处理逻辑，做的是噪声过滤、分组、
去重和截断——保留失败，丢掉那些从来不影响决策的部分。

```
go test ./...        →   126 tests passed
5,000 行                 FAIL: TestXYZ  xxx.go:137  expected 10, got 11
```

**什么时候用。** 报告里「Bash 输出」这一行很大，或者 R009 / R016 触发了。

**怎么验证。** 照常跑一次会话，然后 `aiprobe analyze`，对比「Bash 输出」这一行和工具表。

**它的数字是什么意思。** 项目方声称能减少 60–90%，开销低于 10ms。要仔细读：那是
**bash 输出**的比例，而且项目方自己明确说了——bash 输出只是你上下文中的一类，不是你的
账单。如果 bash 输出占你 prompt 的 20%，那里砍掉 90%，整体是砍掉 18%。

**什么时候别用。** 当你需要的是完整输出而不是「agent 下一步决策所需」的时候——比如
CI 产物，或者你自己要 grep 的日志。

### headroom

[github.com/headroomlabs-ai/headroom](https://github.com/headroomlabs-ai/headroom)
· [docs.headroomlabs.ai](https://docs.headroomlabs.ai/)

一个夹在应用和 provider 之间的上下文优化层。有两个想法无论你用不用它都值得借走：

**内容感知压缩。** JSON、代码、日志、diff、HTML 和自然语言，各自适合的压缩规则完全不同。
通用的截断把它们都当纯文本处理，于是丢掉的往往正好是该留的那一半。压缩一个 JSON 数组时
保留 schema 加几行样本，模型需要的信息还在；直接截到 2,000 字符就没了。

**CCR —— 压缩、缓存、取回。** 发出去的是压缩版，原文按 hash 存在本地，同时给模型一个
工具，让它在确实需要时把原文取回来。这一步是让「激进压缩」变安全的关键：信息没有消失，
只是隔着一次调用。而且按 hash 做内容寻址意味着压缩结果是稳定的，这对 prompt 缓存很重要
（见[让前缀保持稳定](#让前缀保持稳定)）。

**什么时候用。** 「工具结果」或「文件读取」这两行很大，R008 或 R010 触发了，或者你自己
在做网关、想参考这套架构。

**怎么验证。** 对比前后的「工具结果」和「文件读取」两行——**同时确认答案仍然正确**。
改变了结论的压缩不叫节省，这也是所有有损层都要盯的失败模式。

**它的数字是什么意思。** 项目方声称能减少 60–95% 的 token，分负载的数字是：SRE 故障
排查 92%、GitHub issue 分类 73%、代码搜索 92%。那些是项目方在自己负载上的测量结果。
把它们当作「这个方法有效」的证据，而不是对你的预测。

### Capsule

专门面向 Codex、MCP 这类 agent 运行时的上下文压缩，覆盖终端输出、文件、项目上下文和
网页，并且和 CCR 一样强调可精确还原。

比上面两个更新、验证更少。如果你的浪费集中在 agent 自己产生的上下文（而不是 shell
输出）上，值得关注。

**什么时候用。** R016 触发了——某个工具是靠累积而不是靠单次大结果主导上下文的。

### 先定位再读取

这不是一个项目，是一个习惯，而且是大多数人一个下午就能改完、收益最高的一件事。

为了找一个函数把整个文件读进来，是 agent 会话里最常见的可避免浪费，而且它贵两次：
一次是 token 本身，另一次是这个文件会在之后每一次请求里继续留在上下文里。

```sh
rg -n 'func Login' internal/          # 先拿到行号
# 然后只读一个窗口：offset ≈ 命中行 - 40，limit ≈ 120
```

- [ripgrep](https://github.com/BurntSushi/ripgrep) —— 快到「先定位」成了自然习惯，
  而不是多出来的一步。
- [ast-grep](https://github.com/ast-grep/ast-grep) —— 按语法而不是按文本搜索，可以
  找到一个函数的所有调用点，而不必打开包含它们的文件。

然后把这个习惯写进 agent 的规则文件，不要靠自己记：

```
读之前先用 Grep 定位。读范围（offset/limit），不要读整个文件。
不要为了「回忆」而重读文件；它已经在对话里了。
```

**怎么验证。** 看「文件读取」这一行，以及 R011 里报告的整份读取次数。

## prompt 压缩

*第 ④ 层。* 这一层针对的是**每一次请求都会带上**的东西：系统提示、你的规则文件、工具
schema。它比工具输出小，但它每一轮都要付，这就是 R012 存在的原因。

按这个顺序处理，因为便宜的选项同时也是安全的选项：

1. **删。** 大多数规则文件里都有几段从来没改变过模型行为的话。砍到只剩真正有用的。
2. **移。** 参考资料应该放在文件里让 agent 按需去读，而不是放在开场白里。
3. **卸。** 每个已加载工具的 schema 每轮都会重发。这就是 MCP 工具税，而关掉一个服务器
   比压缩文本收益更大、风险更小。
4. **最后才压缩**，用下面这些工具。

### LLMLingua

[github.com/microsoft/LLMLingua](https://github.com/microsoft/LLMLingua) —— 微软。
三个相关的东西：

- **LLMLingua** —— 用一个小模型给 token 的重要性打分，丢掉低价值的那些。输出读起来
  怪，但模型通常仍然能理解。
- **LongLLMLingua** —— **query-aware**：相对于「当前在问什么」来压缩检索到的文档，
  也就是先排序再裁剪，而不是一律缩小。做 RAG 的话，这个才是关键。
- **LLMLingua-2** —— 换成类似 token 分类的做法；据称比第一代快若干倍，一般应该从
  这个开始。

**公开的声称，以及实际该期待什么。** 微软报告 LLMLingua 最高约 20 倍压缩，
LongLLMLingua 在长上下文和 RAG 任务上大约只用四分之一的 token。生产环境里，把
100k 压到 50–70k 是个理性的目标；对你真正依赖的正文追求 20 倍压缩，就是丢掉你需要的
信息的方式。

**什么时候用。** R012 触发了，而且你的固定开销主要来自删不掉的正文。

**什么时候别用。** 代码、结构化输出，以及任何 token 必须精确的地方。从一段 JSON 或
diff 里丢掉「低信息量」token，等于把它弄坏了。

**怎么验证。** 压缩规则文件，重新分析，对比「每次请求的固定开销」。2 倍就算不错的结果。

### Selective Context

[github.com/liyucheng09/Selective_Context](https://github.com/liyucheng09/Selective_Context)

同一类做法，判据不同：它丢掉**自信息（self-information）**低的词——越容易被预测的词，
携带的信息越少。项目的定位大致是「同样的窗口能装大约两倍的内容」。

学术上有意思；但如果今天要上生产做 prompt 压缩，LLMLingua-2 是更实际的起点。

### PCToolkit

[github.com/3DAgentWorld/Toolkit-for-Prompt-Compression](https://github.com/3DAgentWorld/Toolkit-for-Prompt-Compression)

它不是压缩器，而是一个把 LLMLingua、LLMLingua-2、Selective Context、SCRL、KiS 放到
同一个接口和同一套指标下的评测框架。

如果你正在考虑自己做一层压缩，这应该是你的第一站。**不要**根据论文里的 benchmark 选
方法；用 PCToolkit 跑**你自己的** prompt，在你自己的任务上对比压缩率和质量损失。结论
经常是「规则压缩就够了」——而这个结论最好在写 ML 流水线之前就拿到。

## 历史压缩

*第 ⑤ 层。* 到某个时刻，对话本身就成了报告里最大的一项。R017 触发的正是这件事，而任何
工具输出压缩都解决不了它，因为成本来自累积起来的整条轨迹。

这个做法为什么比「逐条消息压缩」更有效：跑了一段之后，把整条轨迹替换成一份书面小结
——发现了什么、还有什么没解决、当前计划是什么——然后从这份小结继续。把 300 条观察各压
20%，你手上还是 300 条观察。

**ReSum**（阿里）是这个思路针对长周期 agent 的完整版本，面向那些会连续跑几百步、否则
会被自己的历史拖死的 agent。

你不需要任何框架就能用上它：

- 在任务边界结束会话，而不是让一个会话开一整天。
- 在运行时替你压缩之前，自己把交接内容写进一个文件，让下一个会话去读。这样摘要是你写的，
  留下的就是你在意的东西。
- 一个任务一个会话，对 prompt 缓存也更友好，因为前缀不再反复变动。

**怎么验证。** 看**下一个**会话里的「对话」这一行和「会话结束时的对话量」。

## 让前缀保持稳定

这根本不是压缩——这是最便宜的一层，也是最常被无意破坏的一层。provider 对缓存命中只收
输入价格的一小部分，而命中的前提是 prompt 的开头和上次**逐字节相同**。

会把缓存丢掉的事情，AIProbe 在缓存原因表里都会明确归因：

- 会话中途改规则文件（R007）
- 会话中途切模型——缓存条目是按模型隔离的（R006）
- 空闲时间超过缓存 TTL（R005）
- 常驻上下文里放时间戳、运行 id 或「当前状态」块（R007）
- 上下文压缩，它在设计上就会让前缀失效（R013）

修法是行为上的，而且免费：把易变的内容放到 prompt **末尾**，让开头保持稳定，尽量连续
地工作。会让前缀变得不稳定的压缩，可能比它省下的更贵——这也是为什么按内容寻址的压缩
（上面的 CCR）比那些每轮渲染结果都不一样的做法更合适。

## ①② 缓存：干脆不发这次请求

对一个人用编码 agent 来说，这两层重要性较低——你的问题很少重复。但对一个团队在网关后面
用，它们可能胜过其他所有层，因为缓存命中的成本是零。

- **[GPTCache](https://github.com/zilliztech/GPTCache)** —— 最知名的开源语义缓存。
  把请求做 embedding，如果之前答过足够接近的，就直接返回那个答案。
- **[LiteLLM](https://github.com/BerriAI/litellm)** —— 一个网关，自带路由、成本统计、
  fallback、限流，以及精确缓存和语义缓存（基于 Redis）。如果你是在做网关而不是在研究
  压缩，从这里开始；其余的管道也一并有了。

**要记住的风险。** 语义缓存回答的是一个**仅仅相似**的问题。对「测试环境怎么申请」这
没问题。但只要两个相似问题之间的差别本身就是重点，那它就是一个「附带成本节省的正确性
bug」。相似度阈值调保守一点，并且把它命中的内容记下来。

## ⑥ 模型路由

简单的轮次走小模型，难的轮次走大模型。注意它做了什么、没做什么：它降低的是每 token
单价，不是 token 数量。两者都省钱，但只有一个会在 token 报告里体现出来。

[vLLM Semantic Router](https://github.com/vllm-project/semantic-router) 是值得看的
开源实现。要留意它和第 ① 层的相互作用：切模型会让 prompt 缓存失效，所以在**同一个对话
内部**做路由，可能比它省下的更贵。在会话之间路由，不要在会话内部路由。

## 除非你自己托管模型，否则这不是你的问题

**[NVIDIA KVPress](https://github.com/NVIDIA/kvpress)** 压缩的是推理服务**内部**的
KV cache——也就是保存已处理 token 的 attention key/value 的那块显存。NVIDIA 举的例子
说明了量级：一个 70B 模型在 100 万 token 上下文下，FP16 的 KV cache 可能需要几百 GB。
KVPress 实现了 SnapKV、StreamingLLM、PyramidKV、TOVA、KVzip 等方法来决定淘汰什么。

如果你跑 vLLM 或者自建 Llama / Qwen，这确实重要。如果你调托管 API，完全无关：你付的是
token 的钱，不是 provider 显存的钱。

## 如果只做三件事

1. **别再整份读文件。** 用 `rg` 定位，读一个范围，并把这条写进规则文件。零成本，而且
   它通常就是报告里最大的一行。
2. **在 shell 输出前面加一层确定性过滤。** 用 rtk，或者就用 `| tail -20` 和 `--quiet`。
   不需要模型，没有质量风险。
3. **在任务边界结束会话。** 一个跑了几百次请求的会话，每一次请求都在为全部历史付费。

然后重新跑一次 `aiprobe analyze`，看剩下的浪费落在哪一层。

## 关于这份文档里的数字

上面每一个归属到某个项目的百分比，都是**该项目自己公布的声称**，在它自己的负载上测得，
文中已逐处标明。这些数字都没有在这里被独立复现过。

AIProbe 不会安装、调用、打包或为这些工具背书。列出它们是因为它们针对的是 AIProbe 能
测量的模式，而且上面每一处引用都配了一个你事后可以自己做的测量。如果某个工具对你的负载
没用，报告会显示出来——这正是重点。

## 来源

- rtk —— [github.com/rtk-ai/rtk](https://github.com/rtk-ai/rtk)
- Headroom —— [github.com/headroomlabs-ai/headroom](https://github.com/headroomlabs-ai/headroom)、[docs.headroomlabs.ai](https://docs.headroomlabs.ai/)
- LLMLingua / LongLLMLingua / LLMLingua-2 —— [github.com/microsoft/LLMLingua](https://github.com/microsoft/LLMLingua)
- Selective Context —— [github.com/liyucheng09/Selective_Context](https://github.com/liyucheng09/Selective_Context)
- PCToolkit —— [github.com/3DAgentWorld/Toolkit-for-Prompt-Compression](https://github.com/3DAgentWorld/Toolkit-for-Prompt-Compression)
- GPTCache —— [github.com/zilliztech/GPTCache](https://github.com/zilliztech/GPTCache)
- LiteLLM —— [github.com/BerriAI/litellm](https://github.com/BerriAI/litellm)
- vLLM Semantic Router —— [github.com/vllm-project/semantic-router](https://github.com/vllm-project/semantic-router)
- NVIDIA KVPress —— [github.com/NVIDIA/kvpress](https://github.com/NVIDIA/kvpress)
- jq —— [github.com/jqlang/jq](https://github.com/jqlang/jq)
- ripgrep —— [github.com/BurntSushi/ripgrep](https://github.com/BurntSushi/ripgrep)
- ast-grep —— [github.com/ast-grep/ast-grep](https://github.com/ast-grep/ast-grep)

以上描述均为对各项目自身文档的转述与精简。内容已按授权要求改写。
