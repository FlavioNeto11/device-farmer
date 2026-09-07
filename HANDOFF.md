# Estado da sessão — retomar daqui

Atualizado em 2026-09-06, depois de fechar o registro e depois empacotar a
coisa para valer — Docker e Kubernetes, rodados, não só escritos. Este arquivo
é o único lugar que reúne o estado; leia-o antes de qualquer coisa.

## Como subir isto, agora

```bash
docker compose up -d          # 56 devices simulados, dashboard em :8420
bash scripts/k8s-up.sh        # o mesmo num Kubernetes local, um comando
bash scripts/k8s-down.sh      # e some sem deixar nada
```

Para uma fazenda de verdade o token não é opcional — a api se recusa a servir
um plano de controle aberto num endereço que a rede alcança:

```bash
export FARM_API_TOKENS='<token>:operator:<voce>'
docker compose --profile farm up -d
```

E se for usar o `ctl` contra o compose, aponte-o: o default dele é
`127.0.0.1:8080` e o compose publica em 8420.

## Onde o repositório está

- `main` = `a910279`, **empurrado para o origin**. Árvore limpa.
- Remote: `https://github.com/FlavioNeto11/device-farmer`
- **Nenhum PR aberto.** 56 merges nesta sessão; 55 PRs mergeados no total, 1
  fechado. Todos `--no-ff`, então o histórico mostra o que cada um trouxe.
  (`gh pr list --state merged` responde 30 por causa do limite padrão dele —
  passe `--limit`.)
- Schema **v21**, contígua (`00001`…`00021`). 213 arquivos Go, **896 funções
  `TestX` — 1 726 testes contando subtestes — em 23 pacotes**, **15 suítes de
  asserção SQL**, **11 arquivos de cenário em `test/e2e`**.
- Nove papéis: `api`, `scheduler`, `reaper`, `recovery`, `jobrunner`, `janitor`,
  `chargepolicy`, `watchdog`, `node` — mais `all` e `demo`, que multiplexam.

### Verificado de ponta a ponta, nesta ordem, contra este commit

```
go build ./... && go vet ./... && gofmt -l .       # limpos, DATABASE_URL VAZIA
go test -count=1 ./...                             # 24 pacotes ok
farmd migrate up  (banco vazio -> v21)             # 21 migrations
test/assertions*.sql                               # 15 suítes, 15 PASSED
go test ./test/e2e/                                # ok, 297 s
scripts/linux-acceptance.sh   (via WSL)            # 55 checks, exit 0
docker build .                                     # 36 MB, contexto 375 kB
docker compose up -d                               # dashboard 200, 56 devices
docker compose --profile farm up -d                # 9 serviços, 401/401/200
bash scripts/k8s-up.sh                             # cluster vivo, e de volta
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

**Controle interativo: a tela de um aparelho, ao vivo, com um dedo humano nela.**
Cinco unidades em paralelo mais o tronco, tudo mergeado em `main`.

O arco inteiro existe e foi dirigido de ponta a ponta:
`GET /api/v1/devices/{id}/screen` splica o stream do scrcpy,
`POST /api/v1/devices/{id}/input` entrega toque, tecla e scroll,
`internal/screen` é dono da sessão (três transportes ADB e um encoder no
telefone), o drawer do dashboard decodifica com WebCodecs, e
`ctl device screen --out` grava o mesmo stream sem navegador.

**Verificado no Chrome, contra o demo em :8420**: a imagem se mexe, 16 eventos
de input chegaram ao dispositivo, `ffprobe` lê 69 quadros de Constrained
Baseline 288x640 no arquivo que o `ctl` gravou, e o `farm.audit_log` tem
`screen.open`/`screen.close` com contagem de input, duração e a frase que diz
o que a sessão **não** fez.

**A regra que governa tudo isso**: uma sessão de tela termina BYTES e nunca uma
lease. Não existe caminho de `internal/screen` nem das duas rotas até
`farm.leases`. Um socket cortado, uma fence que sobe, um deploy que troca o pod,
uma aba fechada — todos terminam a imagem e deixam o dispositivo exatamente tão
alugado quanto estava. O painel diz isso ao operador na própria mensagem de fim
de stream, porque ele é quem mais provavelmente concluiria o contrário.

**Quatro defeitos que só apareceram porque alguém olhou:**

1. **`test/fakeadb` e `internal/scrcpy` discordavam do formato de fio** — flag
   de config no bit 63 em vez de 62, máscara de PTS de 62 bits em vez de 61, e
   nenhum codec id separado. Cada um era consistente consigo mesmo e nada lia um
   com o outro, então o fake certificava um protocolo que nenhum telefone fala.
   Resolvido contra `app/src/demuxer.c` do Genymobile/scrcpy; o teste duplex do
   `internal/adbwire`, que afirmava o layout errado por offset de byte, foi
   corrigido junto.
2. **O socket de vídeo morria com a chamada que o abriu.** A resposta vinha 200,
   com session header e frame size corretos, e nunca chegava um quadro: os
   sockets eram abertos com o contexto do *spawn*, e o tempo de vida de um stream
   do adbwire É o contexto com que ele foi aberto. Todos os testes unitários
   passavam por cima disso, porque o `Device` falso ignorava o contexto.
3. **`display: block` derrotava o atributo `hidden`** — fechar a tela deixava o
   último quadro na página e uma fileira de botões que pareciam vivos e não
   mandavam nada. Regra de autor ganha da do user agent, independente de
   especificidade.
4. **Um teste apostava no buffer de recepção do kernel** e perdia no Linux — a
   plataforma onde isto roda — enquanto passava aqui.

**As três unidades paralelas que não eram a tela**, cada uma um defeito real que
o registro não conhecia:

- **`internal/fenceproxy`**: com o proxy ligado, o `/exec` da api, a sonda de
  bateria do watchdog e as escritas de brand do enroll estavam **todos recusados,
  em silêncio**. Os literais agora viajam dos pacotes que os possuem; o `/exec`
  recusa com uma mensagem em vez de um erro de dial, porque não existe alfabeto
  seguro para o shell de um operador.
- **watchdog**: um `farm.hosts` vazio é um poll que não achou nada, não um erro
  de inicialização. O papel não conseguia subir numa instalação nova — e estava
  em loop de restart nesta máquina havia 14 horas.
- **`ctl device screen`**: o mesmo stream, gravado, sem navegador.

## Empacotamento — o que a auditoria e as corridas acharam

O Dockerfile, o compose e o chart já existiam e eram bons. **Ninguém nunca
tinha rodado nenhum dos três.** Uma auditoria de 54 agentes achou 69 defeitos
verificados (6 bloqueadores, 18 major); as corridas ao vivo acharam os mesmos
bloqueadores por conta própria, o que é a única forma de confiar nos dois.

Os três que impediam qualquer uso, e todos eram o mesmo mecanismo:

1. **`docker compose up -d`** — a primeira linha do próprio arquivo — deixava o
   `demo` em loop de restart. Ele faz bind em `0.0.0.0` e a api se recusa a
   servir aberta num endereço que a rede alcança. A recusa está certa e nomeia
   este deployment exato; o manifesto nunca ligava a escotilha que ela nomeia —
   e `internal/api/auth_test.go` afirmava, num comentário, que ligava.
2. **`--profile farm`** falhava igual com a resposta oposta e correta: um token.
   `FARM_API_TOKENS` não era interpolado em lugar nenhum, então a promessa do
   `.env` de que credenciais vivem "no shell que roda o compose" não alcançava
   nada. Não dá para usar `${VAR:?}` aqui: o compose interpola o documento
   inteiro **antes** de filtrar por profile, então uma variável obrigatória num
   serviço do farm quebra todo comando do projeto, inclusive o do demo. Está
   escrito no arquivo para ninguém "consertar" de volta.
3. **`helm install`** com só um DSN instalava uma fazenda quebrada: cinco
   minutos de `--wait` e um timeout que não menciona token nenhum — enquanto o
   `values.yaml` documentava essa recusa exata um campo acima do valor que a
   causa. Agora recusa no render, em menos de um segundo, com a saída na
   mensagem.

O que só apareceu rodando:

- **`chargepolicy` estava no chart e não no compose**, então a fazenda do
  compose e a do chart não eram a mesma fazenda — e a banda 40–80% (a única
  mitigação de incêndio que software alcança) não era segurada.
- **`trunc 63` cortava o sufixo do componente**, não o prefixo. Passado um
  comprimento de release, nove Deployments renderizavam com um nome só —
  primeiro sumindo o watchdog de um host, calado, depois oito workloads —
  **depois** do hook de migração já ter rodado.
- **Seis templates diziam "este papel não serve HTTP".** Todos servem
  `/healthz` em :9090. Mas o conserto óbvio (um `livenessProbe` ali) seria pior
  que o comentário: uma falha de bind de métricas é deliberadamente não-fatal
  ("um reaper que não sobe é o único caminho automático de release fora do ar"),
  e `/healthz` responde 200 vindo de um reaper travado. Ficou `startupProbe`, e
  o motivo está escrito.
- **O contexto de build era de 340 MB**, 91% worktrees de agente, sem
  `.dockerignore`. Hoje: 375 kB.
- **`CGO_ENABLED=0 GOOS=linux go vet ./... && go test ./...`** — o prefixo de
  atribuição vale para **um** comando, então os testes rodavam com CGO ligado
  enquanto o binário ao lado não. Consertar deixou o build mais **rápido**.
- **Uma fazenda viva não sabia dizer que build era.** `-X main.version` escreve
  no pacote `main`, que `internal/api` não vê, então `/api/v1/capabilities`
  respondia `"dev"` para toda imagem já construída.
- **`k8s-up.sh --build` reconstruía e o cluster continuava com o binário
  antigo.** A tag não se move, então o template do pod não muda e nada rola. O
  id da imagem agora vai numa anotação.

### Kubernetes local, nesta máquina

O Kubernetes do Docker Desktop está **ligado** (eu liguei; `KubernetesEnabled`
no `settings-store.json`, backup em `settings-store.json.bak-before-k8s`).
Contexto `docker-desktop`, um nó, v1.36.1, **modo kind** — então ele não
compartilha o image store do daemon. Ele *consegue* puxar imagem local pelo
`desktop-containerd-registry-mirror`, mas kind/k3d/minikube puros não, então
`k8s-up.sh` importa explicitamente e **confere que chegou** em vez de descobrir
por um `ImagePullBackOff`.

O default do chart, `ghcr.io/flaviopadilha/device-farmer/farmd`, **não existe** —
nada neste repositório o publica ainda. O job novo de CI publica no GHCR; até
uma tag sair, um `helm install` com os valores padrão não tem imagem para puxar.

## Estado do registro

`REQUIREMENTS.md` e a aba **Docs → Requirements** estavam sincronizados
célula-a-célula na rodada passada e **tinham se desencontrado de novo em 35
células**. Agora `TestDocsRegisterMatchesREQUIREMENTS` lê os dois e compara cada
célula, então o próximo desencontro reprova o build em vez de chegar à tela.

- **92 de 109** linhas em `met`, **12** em `met` numa dimensão e abertas em
  outra, **4** `decided`, **1** só-Linux.
- **Oito linhas novas**: a área `SCREEN`. A mais recente, `SCREEN-08`, existe
  porque dois pacotes desta árvore tinham ideias diferentes de um formato de fio
  e nada lia um com o outro — o registro agora carrega a exigência de que algo
  cruze entre eles.
- **Uma aberta**: `REC-03` — tiers 3 (`USBDEVFS_RESET`) e 4 (corte de VBUS)
  contra hardware real. `HW-05` é a mesma frase sobre uma coisa mais estreita.
  **Não há telefone nesta máquina**, e nenhuma mudança de código muda isso. Ler
  que uma porta *pode* ser chaveada não é chaveá-la, e o registro não finge o
  contrário.
- Os gaps das sete páginas do Docs estão em **18** (a página `surface` zerou).

## O que fazer ao retomar, nesta ordem

1. **Rodar contra um rack.** É o item 1 do registro, e agora a tela ao vivo
   depende dele também: a linha de comando do servidor scrcpy é construída a
   partir do protocolo e **nunca foi aceita por um aparelho**. A primeira
   execução contra hardware deve ser lida como uma primeira execução.
2. **Um jar de scrcpy de verdade.** O demo empurra um marcador cujos bytes dizem
   que não são um jar, e marca o artefato como já presente porque este fake ADB
   não fala `sync:`. O push em si não é exercitado em lugar nenhum do demo — é
   exercitado nos testes de sync do `internal/adbwire`, e `SCREEN-01` diz isso.
3. **`-race`.** Nunca rodou nesta máquina (não há compilador C). É o buraco de
   cobertura mais barato de fechar em qualquer máquina que tenha gcc — e agora
   há concorrência nova para ele olhar: uma sessão de tela tem três goroutines
   por dispositivo.
4. **`REC-02`, `SEC-04`, `OPS-04`, `DEV-04`, `DEV-05`** — as linhas `met` em
   código e `unverified` em hardware. Todas viram `met` numa tarde com um rack.
5. O resto está ordenado em `REQUIREMENTS.md` → *What the register argues for
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
