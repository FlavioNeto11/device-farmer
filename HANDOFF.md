# Estado da sessão — retomar daqui

Atualizado em 2026-09-06, depois de mergear as quarenta e nove branches desta
sessão e reverificar tudo contra a árvore resultante. Este arquivo é o único
lugar que reúne o estado; leia-o antes de qualquer coisa.

## Onde o repositório está

- `main` = `76aa430`, **empurrado para o origin**. Árvore limpa.
- Remote: `https://github.com/FlavioNeto11/device-farmer`
- **Nenhum PR aberto.** 49 merges nesta sessão, 133 commits, 30 deles por PR —
  o resto foi branch de worktree mergeada direto. Todos `--no-ff`, então o
  histórico mostra o que cada um trouxe.
- Schema **v21**, contígua (`00001`…`00021`). 213 arquivos Go, **896 funções
  `TestX` — 1 726 testes contando subtestes — em 23 pacotes**, **15 suítes de
  asserção SQL**, **11 arquivos de cenário em `test/e2e`**.
- Nove papéis: `api`, `scheduler`, `reaper`, `recovery`, `jobrunner`, `janitor`,
  `chargepolicy`, `watchdog`, `node` — mais `all` e `demo`, que multiplexam.

### Verificado de ponta a ponta, nesta ordem, contra este commit

```
go build ./... && go vet ./... && gofmt -l .       # limpos, DATABASE_URL VAZIA
go test -count=1 ./...                             # 23 pacotes ok
farmd migrate up  (banco vazio -> v21)             # 21 migrations
test/assertions*.sql                               # 15 suítes, 15 PASSED
go test ./test/e2e/                                # ok, 302 s
go test ./internal/... ./cmd/...  (com banco)      # ok
scripts/linux-acceptance.sh   (via WSL)            # 55 checks, exit 0
```

A corrida no Linux é a que vale mais: **kernel 6.18.33.2, PostgreSQL 18.6**, o
binário deste tree subindo de verdade, `topo.Sysfs` lendo uma árvore USB de um
sistema de arquivos real (o modo `0644` do `disable` de cada porta é o sinal do
kernel para VBUS chaveável), e **as 15 suítes contra um segundo major**. O
script globa `test/assertions*.sql`, então uma suíte escrita amanhã entra sem
ninguém precisar lembrar.

E o invariante, medido vivo: 9 falhas de transporte sobreviveram, **nenhum lease
se moveu**, e todo lease encerrado saiu por `completed` — nunca por
conectividade.

## O que mudou nesta rodada

Duas levas. A primeira fechou o registro (catorze unidades); a segunda foram
quatro pendências que a própria verificação encontrou.

**O que o registro não sabia, e três agentes acharam procurando:**

- **`SEC-05` estava aberta por uma porta que ninguém tinha experimentado.** A
  listagem de leases estava limpa, mas `00009` gravava `prior_instance` e
  `new_instance` no `detail` de um evento `lease_reattached`, e
  `GET /api/v1/events` projetava `detail` **verbatim** — republicando
  `lease_id + fence + holder_instance` numa linha só, para um token de operador
  que não tem escopo entre tenants, e direto na timeline do dashboard. O
  comentário em `internal/api/leases.go` que pulava a checagem de tenant no
  renew **se justificava** na afirmação que isso quebrava.
- **`JOB-03` era mais estreita do que o registro dizia, e a receita documentada
  não funcionava.** O status era lido na largada e no reattach; um soak que
  morria na quarta hora ainda dava step verde. E a spec de referência da aba
  Docs mandava conferir com `cat …result` e `expect_exit: [0]`, que julga o
  `cat`.
- **`JOB-10` estava em tensão direta com um requisito irmão**, e nada arbitrava.
  A resolução foi **reter** o veredito em vez de rebaixá-lo: o job fica
  `running`, que é a ausência de veredito, e as duas exigências passam a valer.

**As quatro pendências da segunda leva:**

| | O que era | Onde ficou |
|---|---|---|
| F1 | O dashboard nunca recebia stream numa fazenda com token, porque `EventSource` não manda header | Tíquete de uso único, TTL de 30 s, gasto na primeira apresentação, abre **aquela** rota e nada mais |
| F2 | `/specs/kinds` descrevia um `wait_for` que não existia mais | `00021`, mais um teste que fixa os identificadores dentro da prosa |
| F3 | Uma renovação que falha no jobrunner virava só linha de log | `HolderHooks` ligado — o alerta já listava o jobrunner como publicador e estava cego para ele |
| F4 | `FARM_MIGRATIONS_TABLE=farm.x` num banco novo falhava com `schema "farm" does not exist` | O migrador cria o schema e **anuncia**; num banco já migrado, **recusa** |

### Quatro defeitos que só apareceram rodando

Nenhum deles era visível numa suíte verde, e é por isso que a corrida no Linux
virou parte da receita:

1. **Todo papel entrava em pânico no startup** por um registro duplicado de
   collector, enquanto `go build`, `go vet`, `gofmt` e a suíte inteira ficavam
   verdes — porque ninguém chamava `newRegistry`. Só existia no Linux: o
   process collector do prometheus não descreve nada em outros sistemas.
2. **`topo.Sysfs` nunca tinha executado.** Todo teste de topologia entrega ao
   `FromFS` um `fstest.MapFS`; o binário chama `Sysfs`, que lê por `os.DirFS` e
   tira a chaveabilidade de VBUS do **modo** do arquivo — que um MapFS só sabe
   afirmar.
3. **O dashboard não recebia stream** em nenhuma fazenda com token.
4. **A schema só tinha sido checada contra um major do PostgreSQL.**

E três testes estavam passando pelo motivo errado — um afirmava sobre
`encoding/json` em vez do código, um casava a linha errada do arquivo que
varria, um tinha um relógio que tornava a própria falsificação indetectável. Os
três foram escritos nesta sessão, pelo mesmo processo que depois os pegou. É
para isso que serve falsificar cada asserção.

## Estado do registro

`REQUIREMENTS.md` e a aba **Docs → Requirements** estavam sincronizados
célula-a-célula na rodada passada e **tinham se desencontrado de novo em 35
células**. Agora `TestDocsRegisterMatchesREQUIREMENTS` lê os dois e compara cada
célula, então o próximo desencontro reprova o build em vez de chegar à tela.

- **86 de 101** linhas em `met`, **11** em `met` numa dimensão e abertas em
  outra, **3** `decided`.
- **Uma aberta**: `REC-03` — tiers 3 (`USBDEVFS_RESET`) e 4 (corte de VBUS)
  contra hardware real. `HW-05` é a mesma frase sobre uma coisa mais estreita.
  **Não há telefone nesta máquina**, e nenhuma mudança de código muda isso. Ler
  que uma porta *pode* ser chaveada não é chaveá-la, e o registro não finge o
  contrário.
- Os gaps das sete páginas do Docs estão em **18** (a página `surface` zerou).

## O que fazer ao retomar, nesta ordem

1. **Rodar contra um rack.** É o item 1 do registro e as duas últimas linhas
   esperam por ele. Tudo que leva às duas chamadas está escrito, testado e
   agora **rodado** no Linux; o que falta é o aparelho.
2. **`-race`.** Nunca rodou nesta máquina (não há compilador C). É o buraco de
   cobertura mais barato de fechar em qualquer máquina que tenha gcc.
3. **`REC-02`, `SEC-04`, `OPS-04`, `DEV-04`, `DEV-05`** — as linhas `met` em
   código e `unverified` em hardware. Todas viram `met` numa tarde com um rack.
4. O resto está ordenado em `REQUIREMENTS.md` → *What the register argues for
   next*.

## Defeitos conhecidos e ainda abertos em `main`

- **Nunca rodou contra hardware.** Não afirmar o contrário em lugar nenhum.
- **`-race` nunca rodou nesta máquina** (não há compilador C).
- **`.env` é versionado** (veio com o `--profile` do compose) e o `.gitignore`
  não o cobre. Não tem segredo nenhum hoje e o comentário no topo diz que o
  farmd nunca o lê, mas é o arquivo onde alguém vai colar um `DATABASE_URL` com
  senha.
- Uma mensagem de commit (`a832121`) perdeu a palavra "armed" para uma expansão
  de crase do shell. O conteúdo está certo; a frase ficou com um buraco.
- **`00002` e `00008` foram editadas depois de aplicadas** (a corrida do
  `CREATE ROLE`, PR #49). É deliberado e está dito dentro das próprias
  migrations: goose nunca reexecuta uma migration aplicada, então uma `00023`
  não alcançaria o statement que corre. Nenhum banco já migrado muda — as duas
  formas terminam com os mesmos três papéis existindo.

## Armadilhas do ambiente

- **`make` não existe** nesta máquina. Comandos crus.
- **`DATABASE_URL` precisa ficar VAZIA** durante `go test ./...`. Os pacotes com
  teste SQL pulam sem ela e a suíte passa; **setada e quebrada, o `TestMain`
  derruba o pacote inteiro**.
- **O banco compartilhado de dev precisa estar migrado.** Vários testes de
  `internal/` leem o banco que `DATABASE_URL` aponta em vez de criar um próprio.
  Com ele atrasado, `TestPublishedStepVocabulary` falha dizendo que a *prosa* de
  um step está errada — a mensagem não menciona versão de schema, e custa tempo.
  Rodar `farmd migrate up` contra ele antes de acusar o código.
- **Asserções precisam de banco de rascunho.** Criar, migrar, rodar, derrubar —
  um banco com seed de demo mata as asserções em chave duplicada.
- Postgres de desenvolvimento: `postgres://farm@127.0.0.1:55432/...` (trust,
  `farm` é superusuário — asserções de GRANT têm que passar por `SET ROLE`).
  Subir com `scripts\dev-up.ps1`.
- **Escolher porta livre para demo**: 8420 e 9090 costumam estar ocupadas.
  Usar `FARM_API_ADDR` e `FARM_METRICS_ADDR` juntos. Um listener de métricas que
  não consegue fazer bind **não derruba mais o papel** — ele loga e exporta
  `farm_metrics_listener_up 0`.
- `/.claude/` está no `.gitignore` — `gofmt -l .` da raiz é confiável.
- **Não editar um script de shell enquanto ele roda.** O bash lê o arquivo por
  offset; reescrevê-lo debaixo dele faz a execução continuar no meio de uma
  linha. Aconteceu com `linux-acceptance.sh` nesta sessão, e o erro de sintaxe
  que apareceu não existia no arquivo.
- Heredoc `python - <<'PY'` nesta máquina **come a barra invertida dentro de
  strings** — a barra dupla vira simples e o Go não compila. Crases dentro de
  heredoc `<<'MSG'` do `git commit -F -` **são expandidas pelo shell**, e um
  heredoc grande com crases e aspas pode nem fechar. Para escrever arquivo,
  usar a ferramenta Write ou `cat > arquivo.py <<'PYEOF'` com um script curto.
- Python aqui é o do Windows: `/tmp/x` dentro de uma **string** do script
  resolve para `C:\tmp\x`, mesmo que o Git Bash traduza o mesmo caminho quando
  passado como **argumento**. Usar caminho Windows dentro do script.
- `farm.jobs` exige tenant e queue que existam: no demo são `acme` e `ci`, não
  `default`. O pool é `default`. Um spec precisa de `"version": 1` e o payload
  vai numa chave com o nome do kind (`{"kind":"sleep","sleep":{"duration":"..."}}`).

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
   espaçamento, limitação de carga e detecção precoce — as duas primeiras estão
   em `docs/siting.md`, e as duas últimas no código
   (`internal/chargepolicy` segura a banda 40–80%, `internal/watchdog/swell.go`
   levanta `battery_anomaly` com `rack_slot`, e `DeviceFarmerBatteryAnomaly`
   pagina em cima).
3. **Código de incêndio não é o obstáculo.** IFC Tabela 1207.1.1 dispara em
   20 kWh; 60 aparelhos somam ~1 kWh. O obstáculo é política do operador de
   datacenter, e **isso não foi estabelecido em nenhuma direção** — é pergunta
   para resolver por escrito antes de comprar hardware.

O invariante, para quem chegar sem contexto: **um lease termina quando o job
diz, quando um prazo que o usuário escreveu vence, ou quando um humano o toma de
volta. Nada mais.** Há testes que reprovam o build se vocabulário de alocação
aparecer em `internal/adbwire`, em `internal/recovery/adbactuator.go` ou em
`internal/fenceproxy`. Leia o teste do pacote antes de escrever nele.
