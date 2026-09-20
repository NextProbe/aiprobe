# 为 AIProbe 做贡献

*[English](CONTRIBUTING.md)*

感谢关注。现阶段最有帮助的三类贡献是：bug 报告、其他 agent 的 adapter，以及翻译。

在动手写代码之前，值得先知道：这个项目的范围是刻意做窄的。一个不能帮忙回答*token 花到
哪去了*、*为什么被浪费了*、*该改什么*的功能，无论实现得多好，大概都会被拒——这不是对工
作本身的评价，只是关于边界。

## 环境准备

```sh
git clone https://github.com/NextProbe/aiprobe
cd aiprobe
make check      # gofmt 检查、go vet、完整测试套件
make build      # ./bin/aiprobe
./bin/aiprobe demo
```

需要 Go 1.25 或更高。唯一的依赖是 [cobra](https://github.com/spf13/cobra)；测试只用标
准库。请保持这样——一个人们拿来分析自己 prompt 的诊断工具，应该能在一个下午内被审计完。

`make check` 就是 CI 跑的东西。本地过了，那边就应该过。

## 那些不是风格偏好的规则

有四条属性是由测试而不是 review 强制的，因为它们正是任何人愿意相信这份输出的理由。如果
你的改动触发了其中之一，正确的修法几乎从来不是改那个测试。

**1. 二进制里没有网络客户端。** `TestNoForbiddenImports` 会走一遍导入图，一旦 `net`、
`net/http` 之类出现在一份很短的例外名单之外就失败。README 承诺没有 HTTP 客户端；这个测
试正是让它成为事实而不是一句声明的东西。`internal/serve` 在例外名单上，也是唯一允许监听
的地方——而且只能是回环地址。

**2. 实测、估算、推导、未知这四者保持区分。** 绝不要把 `trace.OptInt` 展宽成一个带零默认
值的普通 `int`，也绝不要为了让总量看起来精确而缩放某个估算值。如果归因做不到，答案就是
`unknown`，而且它必须一路存活到 JSON 里。

**3. 分析路径上没有模型。** 评分和推荐都是确定性的公式与规则。`TestDeterministic` 和
`TestDocMatchesImplementation` 保证评分可复现、并保证
[docs/scoring.zh-CN.md](docs/scoring.zh-CN.md) 与代码一致。如果你改了某个权重或目标值，
请在同一个提交里更新文档——否则构建会失败，这是设计如此。

**4. 引用了输出的文档保持为真。** `TestReadmeHeadlineMatchesDemo`、
`TestTranslatedReadmesStayInStep` 和 `TestCommittedDemoReportIsCurrent` 会把两份 README
和 `docs/demo-report.txt` 与二进制真实打印的内容做比对。一份数字过期的 README 比一份没有
数字的更糟。

如果你的改动改变了 demo 输出，请重新生成提交进仓库的那份副本：

```sh
make build
{ echo '$ aiprobe demo'; echo; ./bin/aiprobe demo --no-color; } > docs/demo-report.txt
```

并用 `./bin/aiprobe demo --no-color` 和 `--no-color --lang zh` 的输出，更新
`README.md` 与 `README.zh-CN.md` 里的头部区块。

## 新增一个 adapter

这是你能加的最有用的东西。一个 adapter 把某个运行时的会话文件转成 `internal/trace`，而
且它是**唯一**被允许知道那个运行时存在的地方。

没有需要实现的 interface。一个 adapter 就是一个暴露两个函数、并接进 `internal/adapters`
的包：

```go
func ParseFile(path string) (*trace.Trace, error)  // parse.go
func Detect(path string) bool                      // discover.go
```

`Detect` 必须在 `adapters.Detect` 里排在 Claude Code **之前**探测，因为 Claude Code 是兜
底项：忘了这一步的 adapter 会被静默当成 Claude Code 解析，产出一份看起来合理但错误的报告。

一个会话不一定只是一个文件。Kiro CLI 会读一个转录文件外加一个指标边车文件；Cursor 则是从
一个所有对话共用的 SQLite 数据库里读出其中一段，用 `path#<id>` 来定位。如果你的运行时属于
后一种，`Discover` 就是负责枚举这些对话的地方，而一个裸路径必须选定其中一段并明说，而不是
把多段混在一起。

解析 JSON 是简单的部分。**有四个问题决定了产出的报告是否诚实**，值得在写任何代码之前先
回答：

1. **这个运行时是否按请求记录 provider token 用量？** 如果是，这些计数就是事实基准：设
   置 `Request.Usage`，下游一切都能是精确的。如果不是，每个数字都变成估算——这是被支持
   的，但请看第 4 点。
2. **它是否记录 prompt 缓存计数器？** 如果不记录，把缓存字段留为零，**并且**确保缓存分
   析器报的是 `unknown` 而不是 0% 命中率。「没有缓存数据」和「你的缓存从没命中」对读者来
   说完全是两回事。
3. **有没有什么内容在进入上下文一次之后又离开了？** 这是最常被答错、也是答错代价最高的
   一个问题。Kiro 会在产生文件写入的那次 prompt 之后丢掉写入体。留下来的内容会被计入之
   后的每一次请求；被丢掉的只计一次。把两者搞混，在开发期间让一个真实会话被高估了约 3
   倍。请把那些事件标为 `Event.Transient = true`。
4. **这个运行时是否公布了任何能用来校验你的估算的东西？** 一个上下文窗口百分比、一个消
   息计数、一个字节总量——任何随上下文大小变化的东西。如果有，就用上：在没有 token 计数器
   的情况下，这个校验就是「估算」和「猜测」之间的区别，而且它必须与假定的窗口大小无关。

   如果它确实什么都不公布，这个适配器仍然值得做，但它必须改变报告所声称的内容，而不是悄悄
   产出一堆看起来一样的数字。Cursor 就是那个实例：没有 token、没有缓存、没有花费、没有窗
   口、没有模型。它的适配器开门见山给出一条警告，说明每个数字都是背后没有任何依据的估算；
   它根本不报花费，而不是报一个定了价的猜测；并且把报告的重量交给内容层面的发现 —— 重复的
   上下文、被读了两次的文件、过大的工具结果 —— 因为不管账单是多少，这些对记录本身都是成立
   的。

   要守住的底线是：「无法验证」必须让读者看得见，而不是只记在能力表里。

在开 pull request 之前：

- [ ] `grep -ri yourruntime internal/analyzer internal/report` 是空的。任何分析器或渲染
      器都不得按名字提到某个运行时。
- [ ] `Capabilities` 描述的是这个格式真正提供的东西，包括那些 `false` 的条目。
- [ ] 检测是结构性的、只读几行、并且不会在其他运行时的 fixture 上误判。
- [ ] 无法解码的行被计数并跳过，绝不致命。
- [ ] 缺失的量是 `OptInt`/`OptFloat` 的未知值，不是零，而且一路到 JSON 都是。
- [ ] 测试覆盖：正常会话、被截断的文件、空文件、以及没有工具调用的会话。
- [ ] **fixture 是合成的。** 绝不要提交真实转录文件：它们含有 prompt 和源代码。
- [ ] 运行时已加入 `--adapter` 的帮助文本、以及 README 里的会话位置表格。

如果你发现自己想在某个分析器内部为你的运行时开特例，那说明 trace 模型缺了一个概念——请把
那个概念作为 issue 提出来，而不是加上那个分支。Kiro 当初正是这样：分析器一行没改，但模型
需要改，因为它没有办法表达「这段内容离开了上下文」。一个说不出这件事的模型不是与 provider
无关的，它只是长得像 Claude 而已。

## 新增或修正翻译

消息目录在 `internal/i18n/catalog_<locale>.go`。新增一种语言就是加一个文件加
`Locales()` 里的一行；随后有一组测试会用与其他语言相同的标准约束它：

- `TestEveryLocaleCoversEnglish` —— 英文定义的每个 id 都必须存在。
- `TestNoLocaleInventsIDs` —— 不得有英文没有的 id。
- `TestPlaceholdersMatch` —— 两边的 `{named}` 占位符必须一致。
- `TestLiteralMessageIDsExist` —— 代码或 HTML 模板查找的每个 id 在每种语言下都真实存
  在。没有这条，一个拼错的 id 会作为正文发布出去。

翻译前需要知道两件事：

- **占位符是命名的，不是位置的**（`{tokens}`，不是 `%s`）。中文和英文的从句顺序不同，所
  以译者必须能把一个值挪到句子需要它的位置。
- **分析器不产出散文。** 它们记录的是一个消息 id 加参数，由报告器渲染。所以一句新的分析
  器文案意味着一个新的目录条目，而不是分析器里一个新的字符串字面量。JSON 报告携带的英文
  渲染来自同一个目录条目，这正是两者不可能漂移的原因。

新增一种语言就是一个新的 `catalog_<locale>.go` 加 `Locales()` 里的一行。有两样东西刻意
**不**翻译：模型名与工具名（它们必须与你会输入的内容一致），以及 JSON 报告里的 `schema`
字段。

对于终端输出，请记住 CJK 字形是双倍宽，而且布局是对齐到一个硬列宽上限的。换行由渲染器处
理，但如果你新增了一个表格列，请在多个宽度下检查：

```sh
for w in 60 76 100 120; do ./bin/aiprobe demo --no-color --lang zh --width $w; done
```

文档是双语的：一个英文文件，旁边一个 `.zh-CN.md`，两者在顶部互相链接。改一个就要改另一
个——一份悄悄落后的翻译比一个没翻译的页面更糟，因为没人分辨得出来。

## 提交与 pull request

- 一个 PR 只做一件事。把重构和行为变更捆在一起，既难 review 也更难回滚。
- 在提交信息里解释*为什么*。diff 已经展示了「做了什么」。
- bug 修复要带一个测试，这样这个 bug 不能悄悄回来。
- 注释应当解释推理，而不是复述代码。这个代码库很依赖注释来记录那些不显然的权衡；请与之
  保持一致。

## 报告 bug

最有用的报告是「一个会话产出了错误的数字」，加上你预期是什么、以及为什么。会话转录文件
含有你的 prompt 和文件内容，所以**不要把它附上来**。取而代之：

- 跑 `aiprobe analyze <session> --json`，只附上错的那部分；或者
- 描述它的形状：多少次请求、哪个运行时和版本、是否涉及 MCP server、数字大概从哪里开始不
  对。

如果解析器跳过了某些行，报告的 `parsing` 那一行会写明——请把那个计数一起贴上。

## 报告安全漏洞

请不要开公开 issue。见 [SECURITY.zh-CN.md](SECURITY.zh-CN.md)。

## 许可

贡献以 [MIT 许可证](LICENSE) 接受，与项目本身条款相同。
