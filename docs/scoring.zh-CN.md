# 上下文效率评分

*[English](scoring.md)*

上下文效率评分是一个 0 到 100 的数字，只回答一个问题：**这个会话付了钱的内容里，有
多少是真正有用的上下文？**

它遵守两条规则：

1. **它是确定性的。** 评分是 provider token 计数器与 AIProbe 自身分析器的闭式函数。
   同一个会话跑两遍，在任何机器上、任何时候，都会得到同一个数字。
2. **没有模型参与。** 子项没有、权重没有、字母等级也没有。语言模型从头到尾看不到这
   个会话，也不会给它打分。这份文档就是完整定义。

实现在 [`internal/analyzer/score/score.go`](../internal/analyzer/score/score.go)。
有一个测试（`TestDocMatchesImplementation`）会在本文档与代码对某个子项的 key、权重
或公式产生分歧时让构建失败。

---

## 子项与权重

| Key | 子项 | 权重 |
| --- | --- | --- |
| `cache_efficiency` | 缓存效率 | 30 |
| `context_uniqueness` | 上下文唯一性 | 25 |
| `mcp_utilization` | MCP 工具利用率 | 15 |
| `tool_output_efficiency` | 工具输出效率 | 15 |
| `prefix_stability` | 前缀稳定性 | 15 |

名义权重合计 100。

下文中的 `clamp01(x)` 指 `min(1, max(0, x))`。

---

## 1. 缓存效率 —— 权重 30

prompt 里有多少是 provider 从缓存里取的，而不是按完整输入价计费的。

```
raw   = cache_read_tokens / prompt_tokens
score = clamp01( cache_read_tokens / prompt_tokens / 0.85 )
```

- `cache_read_tokens` 与 `prompt_tokens` 都是**实测值**，按每次请求累加，分别来自
  `cache_read_input_tokens` 和
  `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`。
- 目标值是 **0.85**。一个正确复用前缀的长会话，缓存读取占比会在 85% 以上——因为对话
  是单调增长的，只有最新一轮是新的。达到目标就是满分；超过目标没有额外奖励，因为剩
  下的部分就是工作本身。

以下情况为「未知」：会话没有记录 prompt token，或者请求少于两次。第一次请求没有可复
用的缓存前缀，它的命中率结构性地就是零，不含任何效率信息。

## 2. 上下文唯一性 —— 权重 25

agent 拉进上下文的工具输出里，有多少是新信息，而不是已有内容的重复。

```
raw   = 1 - duplicate_insert_waste / tool_output_tokens
score = clamp01( ((1 - duplicate_insert_waste / tool_output_tokens) - 0.60) / 0.40 )
```

- `duplicate_insert_waste` 是重复检测器找到的冗余副本的估算 token 成本：重复读取的
  文件、重复的命令输出、以及完全相同或高度相似的工具结果。它只算副本，绝不算第一次
  出现。
- `tool_output_tokens` 是会话中全部工具输出的估算成本。
- 下限是 **0.60**。工具输出的唯一比例低于 60%，说明会话主要在为重复读取自己已有的东
  西付费，这一项得零分；100% 唯一得满分。

会话没有产生任何工具输出时为「未知」。

## 3. MCP 工具利用率 —— 权重 15

加载进来的 MCP 工具集里，会话实际用到了多少。工具定义无论有没有被调用，每次请求都会
重新发送一遍。

```
raw   = tools_used / tools_loaded
score = clamp01( (tools_used / tools_loaded) / 0.50 )
```

- `tools_used` 是至少被调用过一次的不同 MCP 工具数量。
- `tools_loaded` 来自 `--tools` 提供的工具清单。
- 目标值是 **0.50**。加载的工具里有一半有用就算健康；没人会在每个会话里调用每个工
  具，要求更高只会惩罚合理的配置。

以下情况为「未知」：

- 会话中没有涉及任何 MCP server，或者
- 没有工具清单，所以 `tools_loaded` 未知。AIProbe 不会去猜一个分母。

## 4. 工具输出效率 —— 权重 15

工具结果是以可用的大小返回的，还是整块倾倒进来的。一次 4 万 token 的文件读取，会在
之后的整个会话里主导上下文。

```
raw   = 1 - oversized_output_tokens / tool_output_tokens
score = clamp01( ((1 - oversized_output_tokens / tool_output_tokens) - 0.30) / 0.70 )
```

- `oversized_output_tokens` 是单次达到或超过 **4000 token** 的工具结果的估算成本。
  这个阈值可以用 `--oversized-result-tokens` 配置；评分始终使用本次运行生效的值，并
  在报告里写明。
- 下限是 **0.30**。工具输出中超过 70% 来自超大单次结果的会话，这一项得零分。

会话没有产生任何工具输出时为「未知」。

## 5. 前缀稳定性 —— 权重 15

prompt 的开头有没有保持不动。任何改动前缀的东西，都会让它之后的所有缓存条目失效。

```
raw   = 1 - avoidable_cache_miss_tokens / prompt_tokens
score = clamp01( ((1 - avoidable_cache_miss_tokens / prompt_tokens) - 0.70) / 0.30 )
```

- `avoidable_cache_miss_tokens` 是**实测值**：对每次请求，取上一次请求的 prompt 大小
  减去本次请求的 `cache_read_input_tokens`，只在缺口超过上一个 prompt 的
  `max(1024, 3%)` 时计入。
- **归因于上下文压缩（compaction）的漂移会被排除**。压缩是 agent 按设计工作，不是可
  修的配置错误，所以不扣分。
- 下限是 **0.70**。因可避免的缓存未命中损失掉超过 30% 的全部 prompt token，这一项得
  零分。

会话请求少于两次、或没有记录 prompt token 时为「未知」。

---

## 子项如何合成

无法评估的子项会被报为**未知**，它的权重按比例重新分配给那些能评估的子项：

```
known_weight       = 可评估子项的名义权重之和
effective_weight_i = weight_i / known_weight * 100
points_i           = score_i * effective_weight_i
value              = round( points_i 之和 )
```

由于有效权重始终合计为 100，各项得分直接相加就是最终的 0–100 值。

因此会话绝不会因为 AIProbe 观察不到的数据而被扣分。一个没有 MCP server 的项目，不会
因为「没有 MCP 利用率」而被降分；只是其余子项承担更多权重而已。报告始终会说明哪些子
项被重新分配了权重、以及为什么。

如果什么都评估不了，评分会报为 `n/a`，而不是 0。

## 等级

| 分值 | 等级 |
| --- | --- |
| 85–100 | A |
| 70–84 | B |
| 55–69 | C |
| 40–54 | D |
| 0–39 | F |

等级只是帮你快速读数。真正有用的是子项表格，因为它指出哪个改动值得做。

---

## 实测值与估算值

评分混合了实测输入和估算输入，而在你要据此行动时，这个区别是重要的：

| 输入 | 来源 |
| --- | --- |
| `prompt_tokens`、`cache_read_tokens` | 实测。provider 用量计数器。 |
| `avoidable_cache_miss_tokens` | 实测。仅由 provider 计数器推导。 |
| `tools_used`、`tools_loaded` | 精确计数，来自转录文件与提供的清单。 |
| `duplicate_insert_waste`、`tool_output_tokens`、`oversized_output_tokens` | 估算。token 数来自 AIProbe 的离线估算器。 |

估算器是一个确定性的字符类启发式算法，文档见
[`internal/tokenize/tokenize.go`](../internal/tokenize/tokenize.go)。在混合的 agent
流量上误差大约 ±15%；当会话提供了连续的请求时，它会对照实测的 prompt 增长做校准以缩
小误差。它绝不会改写一个实测总量：provider 的数字优先，估算部分也绝不会被缩放以让总
量看起来是精确的。

实际含义：子项 1 和 5 与 provider 自己的账单一样准确。子项 2、3、4 能可靠地给问题排
序，但它们的绝对 token 数是估算值——这也是报告里每个这类数字都带一个前导 `~` 的原因。

## 为什么是这五项

每个子项都对应一个人真正做得到的改动：

| 子项 | 它指向的改动 |
| --- | --- |
| 缓存效率 | 别再让前缀失效；让会话保持温热。 |
| 上下文唯一性 | 别再重复读同一批文件、重复跑同一批命令。 |
| MCP 工具利用率 | 少加载工具，或者改成按需加载。 |
| 工具输出效率 | 在源头就限制输出量；读区间，而不是整个文件。 |
| 前缀稳定性 | 在一个会话期间，保持规则、工具配置和模型不变。 |

一个不能导向上述任一改动的指标，就不会进入这个评分。
