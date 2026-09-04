# Estado da sessão — retomar daqui

Pausado em 2026-09-04. Este arquivo é o único lugar que reúne o estado; leia-o
antes de qualquer coisa.

## Onde o repositório está

- `main` = `e3ac24e`, **empurrado para o origin** (isso foi corrigido durante a sessão).
- Remote: `https://github.com/FlavioNeto11/device-farmer`
- Schema em migration `00007`. ~30k linhas, 279 testes Go, 26 asserções SQL em `main`.
- Plano completo: `C:\Users\Administrator\.claude\plans\depois-de-toda-essa-wise-cloud.md`

## PRs abertos — 9, todos verdes, nenhum mergeado

| PR | Unidade | O que faz | Migration |
|----|---------|-----------|-----------|
| [#1](https://github.com/FlavioNeto11/device-farmer/pull/1) | 10 | Uma linha de heartbeat por host; enrollment para de decair numa farm saudável | — |
| [#2](https://github.com/FlavioNeto11/device-farmer/pull/2) | 8 | Registra os 10 conjuntos `Collectors()`; encerra a colisão de `farm_recovery_attempts_total`. `/metrics`: 15 → 94 nomes | — |
| [#3](https://github.com/FlavioNeto11/device-farmer/pull/3) | 26 | `docs/siting.md` e `docs/hub-validation.md` | — |
| [#4](https://github.com/FlavioNeto11/device-farmer/pull/4) | 1 | Registro de 101 requisitos + área na aba Docs | — |
| [#5](https://github.com/FlavioNeto11/device-farmer/pull/5) | 4 | Leitor de bateria e temperatura no watchdog | `00012` |
| [#6](https://github.com/FlavioNeto11/device-farmer/pull/6) | 3 | Estado "parado de propósito" — `admin_state='parked'` + ledger + role `farm_parker` | `00008` |
| [#7](https://github.com/FlavioNeto11/device-farmer/pull/7) | 9 | Autorização no re-anexo de lease — `holder_principal` | `00009`/`00010` (conferir) |

Além dos 7 da tabela acima:

| PR | Unidade | O que faz |
|----|---------|-----------|
| [#8](https://github.com/FlavioNeto11/device-farmer/pull/8) | 14 | Escada: alcança tier 0, entrega acknowledgement de power domain, conclui o fechamento de quarentena |
| [#9](https://github.com/FlavioNeto11/device-farmer/pull/9) | 17 | Config validada e descartada — `FARM_COMPONENT`, `FARM_LEASE_RENEW_INTERVAL`, `FARM_METRICS_ADDR`, `Summary()`, exit 3 |

## Unidades interrompidas — trabalho PARCIAL RECUPERÁVEL nas worktrees

Foram canceladas no meio, mas **os arquivos estão lá, sem commit**. NÃO
redisparar do zero antes de olhar: em `C:\git\device-farmer\.claude\worktrees\agent-<id>`

| Unidade | id da worktree | arquivos não commitados |
|---------|----------------|-------------------------|
| 2 — corrigir inventário de gaps | `aa7708d5ac` | 6 |
| 5 — charge gate (verbo + rota no agente) | `a419ddf51f` | 5 |
| 12 — holder_instance + resolve_device + bulk | `a6a28ffa5b` | 1 commit + 9 |
| 18 — kill switch do reaper + shell_detached | `a5c1c77743` | 6 |
| 22 — alertas + runbooks | `a174891f1d` | 5 |
| 23 — testes reaper + scheduler | `ac3c21d645` | 7 |
| 24 — testes runner + jobrunner | `ae330c86f6` | 7 |
| 27 — proxy mTLS de fence | `ae83b6e055` | 2 |

Estavam todas na fase final (revisão de código ou teardown), então o grosso do
trabalho está feito. Recuperar com `cd` na worktree, revisar o diff, rodar os
gates e abrir o PR. **Cuidado**: as worktrees de base defasada não têm
`.gitattributes`, então o `git status` mostra ruído CRLF — olhar o conteúdo do
diff, não a contagem de arquivos.

Nunca disparadas: 6 (política de carga 40–80%), 7 (saúde/inchaço de bateria),
20 (knobs do `topo`), 21 (superfície de operador para slots), 25 (testes de
`config`, absorvido pela 17), 28 (integração do proxy de fence).

Nunca disparadas: 6 (política de carga 40–80%), 7 (saúde/inchaço de bateria),
20 (knobs do `topo`), 21 (superfície de operador para slots), 25 (testes de
`config`, absorvido pela 17), 28 (integração do proxy de fence).

## O que fazer ao retomar, nesta ordem

1. **Mergear os 7 PRs.** Conflitos conhecidos e triviais:
   - `Makefile`: as unidades 3 e 9 corrigiram, cada uma por si, o alvo
     `assertions`, que terminava em `| grep … || true` e **engolia falhas** — o
     alvo ficava verde com o protocolo de lease quebrado. Ficar com uma versão.
   - Numeração de migration: hoje é `00008`, `00009`/`00010`, `00012` — com
     buraco. `farmd migrate up` chama goose **sem `WithAllowMissing`**, então
     aplicar fora de ordem trava migrações futuras. **Renumerar para contígua
     antes de mergear.**
2. **Rebasear e reverificar o PR #4.** Foi construído sobre `ae7ac8c`, três
   commits atrás, então a coluna de status está errada para tudo corrigido
   naqueles commits — inclusive a linha que ele elegeu como de maior
   alavancagem, `OPS-04` ("farmd node cannot start"), já corrigida em `b789bf8`.
3. **Corrigir `internal/api/capabilities.go`** — é meu e é o mais preocupante.
4. Redisparar as 10 unidades interrompidas.

## Defeitos conhecidos e ainda abertos em `main`

Três agentes independentes acharam a mesma classe de bug no `capabilities.go`:

- **`handleCapabilities` descarta o erro das três sondas ao banco e devolve 200.**
  Com o Postgres inalcançável a aba Docs mostra um diagnóstico confiante e
  falso: `schema v0`, "run farmd migrate up", todo papel como nunca tendo
  batido — e o 200 suprime a faixa de erro que o dashboard levantaria.
- **`roleStatuses` descarta o erro da query**, então uma falha de consulta faz o
  control plane inteiro se ler como morto.
- **`/api/v1/capabilities` é a única rota sem gate de papel.** Registrei assim de
  propósito (um operador depurando uma auth quebrada precisa ver o estado), mas
  ela entrega o inventário — inclusive o aviso de que a auth está desligada — a
  quem varre a porta, e é a superfície que **continua aberta depois** que a auth
  for corrigida. Reponderar.

Outros, achados pelos agentes e não corrigidos:

- `/metrics` é servido **só pelo papel `api`**. No compose por papéis, o `api`
  publica os contadores dos loops em zero permanente enquanto os processos que
  os incrementam não expõem endpoint — **um scheduler morto se lê como ocioso**.
  O conserto é um listener de métricas por papel (`FARM_METRICS_ADDR` existe,
  validado, e não escuta nada).
- Três "remédios errados" — superfícies que dizem que algo está protegido quando
  não está: a mensagem mandando setar `FARM_API_TOKENS` sem ninguém que a chame,
  o corpo do revoke citando "refused at the host proxy" (proxy não existe), e
  steps de reset reportando `ok` expandindo para zero sub-steps.

## Armadilhas do ambiente

- **`make` não existe** nesta máquina. Comandos crus.
- **`gofmt -l .` na raiz acusa ~942 arquivos** — todos dentro de
  `.claude/worktrees/`, que são cópias das árvores dos agentes. A árvore
  principal está limpa. Rodar de dentro do diretório do projeto, não da raiz com
  worktrees.
- **`DATABASE_URL` precisa ficar VAZIO** durante `go test ./...`. `internal/lease`
  e `internal/janitor` pulam os testes SQL sem ela e a suíte passa; **setada e
  quebrada, o `TestMain` derruba o pacote inteiro**.
- **O banco `device_farmer` na porta 55432 tem o seed do demo** e as asserções
  morrem em chave duplicada contra ele. Criar banco de rascunho, migrar, rodar,
  derrubar.
- Postgres de desenvolvimento: `postgres://farm@127.0.0.1:55432/...` (trust, o
  usuário `farm` é superusuário). Subir com `scripts\dev-up.ps1`.
- Porta 8420 costuma estar ocupada por um demo. Usar outra.

## Contexto que não está no código

Três pesquisas profundas estabeleceram, com fonte primária:

1. **Alugar aparelhos de provedores gerenciados está excluído por contrato**
   para uso contínuo e não-atendido de apps de terceiros. Sauce Labs AUP
   restringe a "legitimate testing or validation" e o ToS §1.1 faz da AUP
   condição da licença; AWS Service Terms §35.2 proíbe root e "install
   persistent software on devices"; BrowserStack §4.3 exige garantia de direitos
   sobre "the application package itself"; Kobiton §2.3 proíbe repassar acesso.
   Hardware próprio é o caminho.
2. **Supressão por gás limpo não detém evento de lítio.** Novec 1230 a 8,5 vol%
   falhou em suprimir **e** em impedir propagação; em nitrogênio puro, sem
   oxigênio e sem chama, propagou mesmo assim. Mitigação é contenção,
   espaçamento, limitação de carga e detecção precoce.
3. **Código de incêndio não é o obstáculo.** IFC Tabela 1207.1.1 dispara em
   20 kWh; 60 aparelhos somam ~1 kWh. O obstáculo é política do operador de
   datacenter, e **isso não foi estabelecido em nenhuma direção** — é pergunta
   para resolver por escrito antes de comprar hardware.

Nunca verificado, e não deve ser afirmado como pronto: `internal/node` e
`internal/topo` **nunca rodaram contra hardware real** (são Linux por natureza),
e `-race` nunca rodou nesta máquina (não há compilador C).
