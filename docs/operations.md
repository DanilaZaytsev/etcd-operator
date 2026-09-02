# Эксплуатация

Руководство для того, кто держит `EtcdCluster` в продакшене. Предполагается уверенное владение k8s и работающая установка оператора ([установка](installation.md) закрывает развёртывание). За «почему» отсылает к [концепциям](concepts.md).

## Ежедневный обзор

Два запроса, которыми вы будете пользоваться чаще всего:

```sh
# Взгляд на уровне кластера: реплики, число готовых, cluster ID, условия.
kubectl get etcdcluster.etcd-operator.cozystack.io -A
kubectl get etcdcluster.etcd-operator.cozystack.io <name> -n <ns> -o yaml

# Взгляд на уровне членов: флаг бутстрапа, спящее состояние, готовность.
kubectl get etcdmember.etcd-operator.cozystack.io -n <ns> -o custom-columns=\
'NAME:.metadata.name,BOOTSTRAP:.spec.bootstrap,DORMANT:.spec.dormant,READY:.status.conditions[?(@.type=="Ready")].status,MEMBERID:.status.memberID'
```

Имена членов — это случайные суффиксы, назначаемые apiserver (`<cluster>-<5 символов>`); не зашивайте их в скрипты. Всегда используйте метку кластера:

```sh
kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>
kubectl get pvc -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>
```

Чтобы обратиться к etcd напрямую, выберите любой под по метке и зайдите в него:

```sh
POD=$(kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns> \
  -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n <ns> "$POD" -- etcdctl --endpoints=http://localhost:2379 \
  endpoint health --cluster
```

## Масштабирование

Оператор фиксирует цель на первом же reconcile, которое увидело новую спецификацию, — см. [схему фиксации намерения](concepts.md#схема-фиксации-намерения). Правки спецификации во время незавершённого reconcile замечаются, но в дело не идут, пока цель не достигнута или не истёк срок.

### Увеличение

```sh
kubectl patch etcdcluster.etcd-operator.cozystack.io <name> -n <ns> --type=merge \
  -p '{"spec":{"replicas":5}}'
```

Оператор добавляет по одному члену как learner, ждёт, пока тот сообщит `Ready=True`, и повышает его перед добавлением следующего. Каждый шаг гейтится готовностью предыдущего learner. На свежем кластере без данных каждый шаг завершается со стороны etcd намного быстрее секунды, и общее время определяется каденцией reconcile оператора (примерно 30 с до повтора); на кластерах с заметным объёмом данных доминирующим фактором становится синхронизация learner (etcd должен передать data-dir, прежде чем `MemberPromote` будет принят). Следить за ходом:

```sh
kubectl get etcdcluster.etcd-operator.cozystack.io <name> -n <ns> \
  -o jsonpath='{.status.readyMembers}/{.status.observed.replicas}{"\n"}'
```

### Уменьшение

```sh
kubectl patch etcdcluster.etcd-operator.cozystack.io <name> -n <ns> --type=merge \
  -p '{"spec":{"replicas":3}}'
```

Жертвой выбирается самый недавно созданный член (`CreationTimestamp` по убыванию, при равенстве — имя по убыванию). Финализатор вызывает `MemberRemove` у оставшихся соседей, прежде чем под и PVC будут собраны сборщиком мусора. Особой защиты сида нет: сид (первоначальный член бутстрапа) не имеет постоянной особой роли и может быть удалён как любой другой.

### Пауза (масштабирование в ноль)

```sh
kubectl patch etcdcluster.etcd-operator.cozystack.io <name> -n <ns> --type=merge \
  -p '{"spec":{"replicas":0}}'
```

Для кластера с N>1 это поэтапный спуск: каждый промежуточный шаг (`MemberRemove` плюс сборка пода и PVC), пока не останется один член, а затем переход 1→0 — «пауза»: у уцелевшего члена `spec.dormant` патчится в `true`. Под исчезает; PVC остаётся во владении `EtcdMember`, который сам продолжает существовать. `etcdctl` снаружи больше недоступен (пода нет), но данные целы.

Наблюдаемое состояние после паузы:

```sh
# Один CR EtcdMember с spec.dormant=true:
kubectl get etcdmember.etcd-operator.cozystack.io -n <ns> \
  -o custom-columns=NAME:.metadata.name,DORMANT:.spec.dormant

# Один оставшийся PVC (data-<имя-члена>):
kubectl get pvc -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>

# Подов нет:
kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>

# Available=False, Reason=Paused, в сообщении назван PVC:
kubectl get etcdcluster.etcd-operator.cozystack.io <name> -n <ns> \
  -o jsonpath='{.status.conditions[?(@.type=="Available")]}{"\n"}'
```

### Возобновление (масштабирование до 1 и выше)

```sh
kubectl patch etcdcluster.etcd-operator.cozystack.io <name> -n <ns> --type=merge \
  -p '{"spec":{"replicas":3}}'
```

Контроллер кластера замечает спящего члена, патчит `spec.dormant=false`, и следующий проход контроллера члена пересоздаёт под на существующем PVC. Etcd читает data-dir и продолжает работу с **теми же cluster ID и member ID**, что и до паузы, — проверить:

```sh
kubectl get etcdcluster.etcd-operator.cozystack.io <name> -n <ns> -o jsonpath='{.status.clusterID}'
# Должно совпасть со значением, которое вы видели до паузы.
```

Дальнейшее увеличение (в примере 1→3) идёт обычным порядком от этой одночленной отправной точки.

## Пулы хранения: на каком массиве какой член

Кластер с `spec.storage.pools` разносит своих членов по нескольким хранилищам, по одному на пул. Раскладка — это метка, а не поле спецификации, которое нужно вычитывать:

```sh
kubectl get etcdmember -n <ns> -l etcd-operator.cozystack.io/cluster=<cluster> \
  -L etcd-operator.cozystack.io/storage-pool
```

```
NAME          ...   STORAGE-POOL
my-etcd-4kx9  ...   array-a
my-etcd-p2mn  ...   array-b
my-etcd-zt7q  ...   array-c
```

Та же метка стоит на поде и PVC каждого члена, поэтому `kubectl get pvc -l etcd-operator.cozystack.io/storage-pool=array-b` прямо отвечает на вопрос «что сейчас держит этот массив».

### Когда массив отказывает

Члена на мёртвом массиве оператор рано или поздно заменит — и замена окажется на *том же* массиве, потому что теперь этот пул наименее занят. Её PVC после этого навсегда остаётся в `Pending`. Это не баг, который надо обходить; это то, ради чего существует кордон.

```sh
# Перестать ставить новых членов на отказавший массив. Существующие члены и
# их PVC не трогаются.
kubectl patch etcdcluster <cluster> -n <ns> --type=json \
  -p='[{"op":"add","path":"/spec/storage/pools/1/disabled","value":true}]'
```

Следующая замена уйдёт на здоровый пул. Посмотреть, куда она села:

```sh
kubectl get etcdmember -n <ns> -l etcd-operator.cozystack.io/cluster=<cluster> \
  -L etcd-operator.cozystack.io/storage-pool -w
```

Когда массив вернётся, снимите `disabled`. Ничто не мигрирует само — уже размещённые члены остаются там, где стоят, — поэтому раскладка будет перекошена, пока достаточное число членов не заменится по другим причинам. Чтобы перебалансировать осознанно, удаляйте по одному члену за раз (через [процедуру разбития стекла](#сломанный-член) и только пока держится кворум) и позвольте механизму заполнения пробелов поставить замену в ставший наименее занятым пул.

`disabled` — единственное изменяемое поле пула. Удаление пула, его переименование или перенаправление на другой `StorageClass` apiserver отклоняет: его PVC уже привязаны. Добавление нового пула разрешено.

Если под кордоном оказались все пулы, создание члена падает с ошибкой, называющей их все, — оператор не станет молча откатываться на `StorageClass` по умолчанию.

## Условия: что они значат и что делать

Полная таблица — в [концепциях](concepts.md#условия). Практически значимое подмножество:

### `Available=False/Paused`

Ожидаемо при `spec.replicas=0`. Посмотрите в сообщении, сохранены ли данные:

- «data is preserved on PVC data-`<name>`» → спящий член существует; увеличение реплик возобновит тот же кластер etcd.
- «no data has been written (cluster never bootstrapped)» → кластер с самого начала создавали с `replicas=0`; увеличение запустит свежий бутстрап.

### `Available=False/QuorumLost`

Готовы меньше половины от `observed.replicas`. Кластер не может обслуживать записи. Проверьте:

```sh
kubectl get etcdmember.etcd-operator.cozystack.io -n <ns> \
  -o custom-columns=NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status
kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns> -o wide
kubectl describe pod -n <ns> <нездоровый-под>
```

Частые причины: застряло связывание PVC (ни одна нода не подходит под топологию storage class), ноду вытеснили без перепланирования, etcd убит по памяти (посмотрите `kubectl logs --previous`), не работает разрешение DNS внутри сети кластера.

Если кворум восстановим (поды вернутся), кластер вылечится сам. Если у члена пропал data-dir, см. [Сломанный член](#сломанный-член).

### `Available=False/ClusterUnreachable`

Обнаружение при бутстрапе не смогло подключиться до сида. Сообщение приходит через `stableErrorMessage(err)`, который убирает изменчивые от вызова к вызову части (метки времени, номера портов), поэтому одна и та же первопричина читается одинаково при повторах.

```sh
kubectl get etcdcluster.etcd-operator.cozystack.io <name> -n <ns> \
  -o jsonpath='{.status.conditions[?(@.type=="Available")].message}{"\n"}'
kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>
kubectl logs -n <ns> <под-сида>
```

Если под сида `Running 1/1`, а контроллер по-прежнему сообщает `ClusterUnreachable`, подозревайте проблему со Service или DNS:

```sh
# Headless-service существует?
kubectl get svc -n <ns> <cluster>
# Разрешается в IP пода?
kubectl run dns-debug --rm -it --image=busybox -n <ns> -- \
  nslookup <имя-пода-сида>.<cluster>.<ns>.svc.cluster.local
```

### `Available=False/BootstrapFailed`

Терминально. Срок истёк раньше, чем был зафиксирован `clusterID`. В поде недостроенного сида зашит флаг `--initial-cluster`, и оператор не может изменить его на месте. Восстановление:

```sh
kubectl delete etcdcluster.etcd-operator.cozystack.io <name> -n <ns>
# Дождитесь сборки всех зависимых объектов:
kubectl get etcdmember,pvc -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>
# Как только это вернёт пустоту — создавайте заново.
kubectl apply -f <ваш-манифест-кластера>.yaml
```

Шаг с ожиданием сборки PVC важен: пересоздание до того, как исчезли прежние PVC, приводит к тому, что новый EtcdMember откажется их присваивать (проверка UID в `pvcOwnedBy` не проходит — см. [концепции](concepts.md#модель-api)). Проверка оператора — это защита, и правильный ответ здесь — подождать.

### `Available=False/DeadlineExceeded`

Терминально. Срок истёк уже после бутстрапа (сам кластер здоров; застряла только последняя операция). Оператор паркуется и ждёт правки спецификации:

```sh
# Посмотреть, что застряло:
kubectl get etcdcluster.etcd-operator.cozystack.io <name> -n <ns> -o yaml
# observed показывает, чего оператор пытался достичь;
# spec показывает, что вы просили изначально.

# Приведите spec к разумному значению (часто — верните предыдущее рабочее):
kubectl edit etcdcluster.etcd-operator.cozystack.io <name> -n <ns>
```

Следующий reconcile замечает `spec != observed`, трактует вашу правку как сигнал вмешательства, снимает слепок новой спецификации в `observed`, ставит свежий срок и продолжает.

### `Progressing=True/WaitingForSeed`

CR сида `EtcdMember` уже существует, но контроллер члена ещё не создал его под — это промежуток между созданием CR контроллером кластера и следующим проходом контроллера члена. `kubectl describe pod` здесь **бесполезен**: пода ещё нет, команда вернёт «not found» и скроет реальное состояние. Смотрите на сам CR и на события неймспейса:

```sh
SEED=$(kubectl get etcdmember.etcd-operator.cozystack.io -n <ns> \
  -o jsonpath='{.items[?(@.spec.bootstrap==true)].metadata.name}')
kubectl describe etcdmember.etcd-operator.cozystack.io -n <ns> "$SEED"
kubectl get events -n <ns> --field-selector involvedObject.name=$SEED
```

В нормальной работе это состояние уходит за один-два цикла reconcile. Если оно держится, контроллер оператора заклинило — смотрите его логи (см. [Логи оператора](#логи-оператора)).

Как только под сида создан, `WaitingForSeed` уходит, и проблемы *на стороне пода* (отсутствующий `StorageClass`, ошибка загрузки образа, отказ PodSecurity в допуске `restricted`-совместимой спецификации) всплывают отдельно. С этого момента правильный инструмент — `kubectl describe pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns>`.

## Принудительная эскалация: укоротить срок

Умолчание `progressDeadlineSeconds` — 600 (10 минут). Если reconcile заклинило и ждать окно не хочется, истеките срок прямо сейчас:

```sh
kubectl patch etcdcluster.etcd-operator.cozystack.io <name> -n <ns> --subresource=status --type=merge \
  -p "{\"status\":{\"progressDeadline\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ -d '1 second ago')\"}}"
```

Это немедленно переводит кластер в терминальное состояние ошибки. Дальше — по соответствующей ветке условий выше (удалить и создать заново для стадии до бутстрапа, править спецификацию после него).

## Сломанный член

Восстановление окончательно сломанного члена **на PVC** (например, потерян PVC, выведена нода) сейчас делается вручную. Члены в памяти заменяются автоматически при потере пода — см. [Кластеры в памяти](#кластеры-в-памяти). Для кластеров на PVC предикат `isBroken` остаётся заглушкой; автозамена не подключена (см. [концепции](concepts.md#чего-нет-в-устройстве)).

Ручное восстановление:

```sh
# 1. Определите сломанного члена.
kubectl get etcdmember.etcd-operator.cozystack.io -n <ns>
# 2. Удаление члена по умолчанию запрещено (см. концепции: удаление члена
#    запрещено на допуске) — это то самое намеренное исключение, разблокируйте:
kubectl annotate etcdmember.etcd-operator.cozystack.io <сломанный-член> -n <ns> \
  etcd-operator.cozystack.io/allow-deletion=true
# 3. Удалите его — финализатор выполнит MemberRemove у соседей, затем сборщик
#    заберёт под и PVC. Кворум держится, потому что мы удаляем до добавления.
kubectl delete etcdmember.etcd-operator.cozystack.io <сломанный-член> -n <ns>
# 4. Следующий reconcile контроллера кластера увидит, что текущих меньше
#    нужного, и увеличит состав автоматически: добавится новый член с именем
#    от GenerateName и свежим хранилищем.
```

Шаг с аннотацией — это и есть смысл защиты: такое восстановление намеренно выбрасывает том с данными, и необходимость это напечатать отличает его от той же команды, набранной по ошибке.

Эта последовательность сохраняет кворум, если число голосующих нечётно и сломан только один. Если одновременно сломаны несколько голосующих, кворум потерян и чисто выполнить `MemberRemove` не получится. В этом случае восстановление — удалить EtcdCluster, создать заново и восстановиться из снапшота, см. [Восстановление кластера из снапшота](#восстановление-кластера-из-снапшота). Снапшоты существуют, только если вы их снимали, поэтому настройте [`EtcdSnapshotPolicy`](#регулярные-бэкапы) *до того*, как он понадобится.

## Снятие снапшота

`EtcdSnapshot` снимает разовый снапшот работающего кластера и складывает его в S3 (или на PVC). Оператор запускает Job с собственным образом в роли агента снапшота; тот обращается к клиентскому Service кластера (автоматически учитывая TLS и аутентификацию из `spec.auth`) и загружает снапшот.

```sh
# Secret с учётными данными S3 в неймспейсе кластера. Агент читает ровно эти
# два ключа.
kubectl create secret generic s3-creds -n <ns> \
  --from-literal=AWS_ACCESS_KEY_ID=<key> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret>

cat <<'EOF' | kubectl apply -f -
apiVersion: etcd-operator.cozystack.io/v1alpha2
kind: EtcdSnapshot
metadata:
  name: my-etcd-2026-06-02
  namespace: <ns>
spec:
  clusterRef:
    name: my-etcd
  destination:
    s3:
      endpoint: https://s3.example.com   # эндпоинт MinIO/Ceph тоже подойдёт
      bucket: etcd-snapshots
      key: my-etcd                       # необязательный префикс; агент добавит "<имя>.db"
      region: us-east-1                  # необязательно
      forcePathStyle: true               # MinIO/Ceph обычно этого требуют
      credentialsSecretRef:
        name: s3-creds
EOF

# Дождитесь Complete; status.artifact запишет, куда он лёг.
kubectl get etcdsnapshot.etcd-operator.cozystack.io my-etcd-2026-06-02 -n <ns> -w
kubectl get etcdsnapshot.etcd-operator.cozystack.io my-etcd-2026-06-02 -n <ns> \
  -o jsonpath='{.status.artifact}{"\n"}'
# -> {"uri":"s3://etcd-snapshots/my-etcd/my-etcd-2026-06-02.db","sizeBytes":...,"checksum":"sha256:..."}
```

Для назначения на PVC используйте `destination.pvc.{claimName,subPath}` вместо `s3`. Ровно одно из `s3` и `pvc` обязательно (это проверяет CEL). Снапшот неизменяем: чтобы снять новый, создайте новый `EtcdSnapshot`. Для регулярных снапшотов есть [`EtcdSnapshotPolicy`](#регулярные-бэкапы) — отдельный CronJob не нужен.

> **Имена объектов должны быть уникальны.** Сохранённый объект ключуется по *имени* `EtcdSnapshot` (`<префикс-ключа>/<имя>.db` для S3, `<имя>.db` на PVC). Агент **отказывается перезаписывать существующий снапшот, который написал не он**, — и для S3, и для PVC, — поэтому снапшот, чей ключ или путь уже занят, падает, а не затирает молча более ранний. Давайте каждому снапшоту отдельное имя (описанный выше шаблон с датой это делает) либо отдельный `key`/`subPath` в назначении. (*Повтор* того же `EtcdSnapshot` — исключение: каждый объект помечен UID снапшота — в метаданных объекта S3 или в соседнем файле `<имя>.db.uid` на PVC, — поэтому агент опознаёт собственную прежнюю запись, и повтор идемпотентен.) «Неизменяемость» CRD относится к объекту: версий снапшотов он не ведёт, поэтому повторное использование имени после удаления объекта его заменяет.
>
> **Учётным данным S3 нужен `s3:ListBucket` (или эквивалент), а не только `s3:GetObject`/`PutObject`.** Защита от перезаписи делает `HeadObject` по целевому ключу; при наличии `GetObject`, но без `ListBucket`, S3 (и некоторые политики MinIO/Ceph) возвращает на *отсутствующий* ключ **403 AccessDenied** вместо 404. Отличить это от настоящей проблемы с правами агент не может, поэтому **отказывает** (не делает снапшот), а не рискует перезаписью. Выдайте учётным данным снапшота `ListBucket` на бакет, чтобы HEAD по отсутствующему ключу возвращал 404.
>
> **`InvalidArgument: x-amz-content-sha256 must be UNSIGNED-PAYLOAD, STREAMING-AWS4-HMAC-SHA256-PAYLOAD or a valid sha256 value`.** Некоторые не-AWS S3-совместимые бэкенды (подтверждено на Ceph RGW; отдельные версии MinIO и Cloudflare R2) отклоняют трейлер гибкой контрольной суммы, который свежий `aws-sdk-go-v2` по умолчанию прикладывает к загрузкам. Агент закрепляет политику контрольной суммы запроса S3 в положении «когда требуется» и на клиенте, и на менеджере многочастной передачи, поэтому этот трейлер не отправляет, и загрузки на таких бэкендах проходят. Если вы всё же получаете эту ошибку, ваш образ агента старше исправления — обновите образ оператора.

Если снапшот оказался в `Failed`, посмотрите логи пода Job агента (Job называется `<имя-снапшота>-snapshot` и собирается через несколько минут после завершения по `ttlSecondsAfterFinished`):

```sh
kubectl logs -n <ns> job/my-etcd-2026-06-02-snapshot
```

## Регулярные бэкапы

Разовый `EtcdSnapshot` — это то, что нужно не забыть создать. `EtcdSnapshotPolicy` — это каденция, и именно её стоит настроить заранее: отсутствие бэкапов не проявляется никак до момента восстановления.

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: etcd-operator.cozystack.io/v1alpha2
kind: EtcdSnapshotPolicy
metadata:
  name: my-etcd-backup
  namespace: <ns>
spec:
  clusterRef:
    name: my-etcd
  schedule:
    cron: "0 */6 * * *"        # каждые шесть часов
    timezone: Europe/Moscow    # зона IANA; отсутствие означает UTC
  destination:
    s3:
      endpoint: https://s3.example.com
      bucket: etcd-snapshots
      key: my-etcd/            # ПРЕФИКС; каждый запуск добавляет своё имя
      region: us-east-1
      forcePathStyle: true
      credentialsSecretRef:
        name: s3-creds
  successfulHistoryLimit: 10
  failedHistoryLimit: 3
EOF
```

Обратите внимание: здесь `key` — это **префикс**, в отличие от источника восстановления, где это точный ключ объекта. Каждый созданный снапшот добавляет собственное имя, поэтому запуски никогда не перезаписывают друг друга.

```sh
kubectl get etcdsnapshotpolicy -n <ns>
```

```
NAME             CLUSTER   SCHEDULE    TIMEZONE        SUSPEND   LAST SCHEDULE   LAST SUCCESSFUL   AGE
my-etcd-backup   my-etcd   0 */6 * * * Europe/Moscow             12m             12m               3d
```

`LAST SUCCESSFUL` — та колонка, по которой стоит настраивать алерт. Она берётся из `status.lastSuccessfulTime`, а рядом `status.lastSuccessfulArtifact` записывает, куда именно этот снапшот лёг, — это и подставляют при восстановлении:

```sh
kubectl get etcdsnapshotpolicy my-etcd-backup -n <ns> \
  -o jsonpath='{.status.lastSuccessfulArtifact}{"\n"}'
```

```json
{"uri":"s3://etcd-snapshots/my-etcd/my-etcd-backup-29558640.db","sizeBytes":4194304,"checksum":"sha256:9f2b..."}
```

Приостановить и возобновить, не удаляя политику:

```sh
kubectl patch etcdsnapshotpolicy my-etcd-backup -n <ns> --type=merge -p '{"spec":{"suspend":true}}'
```

При возобновлении может быть создан ровно один — самый недавний — пропущенный тик (с учётом `startingDeadlineSeconds`); более ранние не воспроизводятся никогда. Тик, отброшенный за слишком большое отставание, сообщается предупреждающим событием `MissedSchedule`, а не проходит молча, — `kubectl describe etcdsnapshotpolicy` это место, где всплывает не случившийся бэкап.

### Окна бэкапа на нагруженном родительском кластере

Кластеры, установленные из одного файла values, несут одно и то же выражение cron, поэтому все снимают снапшот в одну и ту же минуту. На сотне с лишним дочерних кластеров это сотня одновременных потоков снапшотов etcd в одно хранилище на каждом тике — родительский кластер чувствует это как всплеск Job, хранилище как всплеск загрузок, а каждый дочерний кластер как задержку на том члене, к которому обратился его снапшот.

Чарт по умолчанию разносит минуту по часу (`backup.spread` в `clusterDefaults`), выводя её из неймспейса и имени каждого кластера: устойчиво между обновлениями, различается между дочерними кластерами, и никто не выбирает числа руками. Неймспейс входит в ключ намеренно — там, где у каждого дочернего кластера свой неймспейс, естественное имя его кластера во всех них одинаково. Посмотреть, что реально досталось дочернему кластеру:

```sh
kubectl get etcdsnapshotpolicy -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,SCHEDULE:.spec.schedule.cron
```

Пишете политики руками? Дайте каждой свою минуту. Различаться должна только минута — интервал может остаться прежним.

### Хранение

`successfulHistoryLimit` и `failedHistoryLimit` вычищают объекты `EtcdSnapshot`. Лимиты раздельны намеренно: серия отказов не должна вытеснить успешные снапшоты, а восстановиться можно только из них.

По умолчанию это вычищает только *объекты*, а сохранённые снапшоты копятся в бакете бесконечно. Чтобы удалять и их:

```yaml
spec:
  deleteArtifactOnPrune: true
```

Удаляется ровно тот объект, который каждый вычищаемый `EtcdSnapshot` записал в `status.artifact`, — по одному объекту на снапшот, и никогда не перечисление содержимого префикса. Если удаление не удалось, `EtcdSnapshot` *сохраняется* и выпускается событие `PruneFailed`: этот объект — единственная запись о том, где лежит артефакт, и придержать один снапшот сверх лимита это меньшее из зол.

Для этого нужен настроенный образ оператора (из него запускается Job удаления). Без него удаление пропускается с событием `PruneUnavailable`, а не молча, чтобы политика, которая выглядит удаляющей и не удаляет, была видна.

## Метрики

Метрики приходят из двух разных мест, и путать их не стоит.

**Оператор** отдаёт на своём эндпоинте состояние каждого кластера, его членов и свежесть бэкапов: собран ли кластер, сколько членов готово из скольких нужно, когда последний снапшот лёг в хранилище. Это взгляд снаружи, и он есть даже тогда, когда сам кластер не отвечает.

**Каждый под члена** всегда отдаёт метрики самой etcd на порту `2381` (открытым текстом, независимо от TLS): задержки fsync, размер бэкенда, смены лидера, число ключей. Это взгляд изнутри, и он пропадает вместе с подом. О том, за чем здесь следить, см. [наблюдение за задержками хранилища](#наблюдение-за-задержками-хранилища).

Ниже — про первый источник.

### Как снимать

Метрики оператора отдаются на том же эндпоинте, что и служебные метрики controller-runtime, — отдельного порта заводить не нужно. Чтобы Prometheus их забирал:

```yaml
metrics:
  serviceMonitor:
    enabled: true
    # kube-prometheus-stack выбирает ServiceMonitor по этой метке. Без неё
    # объект создастся и молча никогда не будет выбран.
    additionalLabels:
      release: kube-prometheus-stack
```

Посмотреть глазами, не поднимая Prometheus:

```sh
kubectl -n etcd-operator-system port-forward deploy/etcd-operator 8080:8080 &
curl -s localhost:8080/metrics | grep '^etcd_operator_'
```

Это работает при `kubeRbacProxy.enabled=false`. С прокси (умолчание) порт закрыт проверкой SubjectAccessReview, и обращаться нужно через него на 8443 с токеном.

### Что отдаётся

Все метрики читаются из объектов в момент снятия, а не накапливаются при reconcile. Практическое следствие: **серия не переживает свой объект** — удалённый кластер просто перестаёт появляться, и дашборд на сотню дочерних кластеров не зарастает теми, кого уже нет.

#### Кластер

| Метрика | Значение |
|---|---|
| `etcd_operator_cluster_info{namespace,cluster,version,storage_medium,cluster_id}` | Всегда 1; смысл в метках. `cluster_id` отвечает на вопрос «какое это воплощение кластера» после восстановления, которое выдаёт новый. |
| `etcd_operator_cluster_bootstrapped{namespace,cluster}` | 1, когда кластер собрался и зафиксировал cluster ID. **Это и есть признак «собран»**: поды могут работать, а кластер так и не сформироваться. |
| `etcd_operator_cluster_members_desired{namespace,cluster}` | Целевое число членов, к которому оператор идёт сейчас. Это *зафиксированная* цель, а не `spec.replicas`. |
| `etcd_operator_cluster_members_ready{namespace,cluster}` | Сколько членов здоровы и обслуживают запросы. |
| `etcd_operator_cluster_condition{namespace,cluster,condition,reason}` | 1 при True, 0 при False. При Unknown серии **нет** вообще. |

Про `members_desired` стоит сказать отдельно: она следует за зафиксированной целью намеренно. Если бы она показывала `spec.replicas`, то сразу после правки спеки метрика выдавала бы «ready 3 / desired 5» — деградацию на здоровом кластере, ещё до того как оператор эту цель принял. Так алерты и учатся, что их можно игнорировать.

Про условия в состоянии Unknown — то же рассуждение с другой стороны: «никто ещё не проверял» это не «проверено и ложно», и склеивать их значило бы дать непроверенному условию удовлетворить алерт, написанный против 0.

#### Члены

| Метрика | Значение |
|---|---|
| `etcd_operator_member_ready{namespace,cluster,member,storage_pool}` | 1, когда отдельный член здоров. `storage_pool` — метка, потому что во время отказа массива вопрос звучит именно так, и ответ не должен требовать join. |
| `etcd_operator_member_voter{namespace,cluster,member}` | 1 для голосующего, 0 пока член остаётся learner. |

#### Бэкапы

| Метрика | Значение |
|---|---|
| `etcd_operator_snapshot_last_success_timestamp_seconds{namespace,cluster}` | Когда последний снапшот кластера успешно лёг в хранилище. |
| `etcd_operator_snapshot_last_success_age_seconds{namespace,cluster}` | Сколько секунд прошло с тех пор. То же самое, но вычитание уже сделано, чтобы алерту не требовалось знать время скрейпа. |
| `etcd_operator_snapshot_last_success_size_bytes{namespace,cluster}` | Размер последнего успешно сохранённого снапшота. |
| `etcd_operator_snapshots{namespace,cluster,phase}` | Сколько объектов EtcdSnapshot в каждой фазе. Это хранимая история, а не скорость: число ограничено лимитами политики. |

Три первые метрики ключуются на **кластере**, а не на политике, намеренно: «бэкапился ли этот дочерний кластер недавно» имеет один и тот же ответ независимо от того, пришёл ли снапшот от политики, от разового `EtcdSnapshot` или от политики, которую с тех пор удалили.

И главное: у кластера, который **ни разу** не бэкапился, этих серий нет вовсе. Не ноль. Ноль — валидный Unix-timestamp, а возраст 0 читается как «только что забэкаплен», то есть ровно наоборот. Ловить такой случай нужно через `absent()` или `unless` — пример ниже.

#### Политики бэкапов

| Метрика | Значение |
|---|---|
| `etcd_operator_snapshot_policy_info{namespace,policy,cluster,schedule,timezone}` | Всегда 1. `schedule` — то расписание, по которому политика реально работает, включая минуту, разнесённую чартом по часу. Иначе на вопрос «почему это сработало в :37» из мониторинга не ответить. |
| `etcd_operator_snapshot_policy_suspended{namespace,policy}` | 1, пока политика приостановлена. Приостановленная политика не сбоит, поэтому в алерт на устаревание бэкапа она не попадёт — из-за чего ей и нужна отдельная серия. |
| `etcd_operator_snapshot_policy_last_schedule_timestamp_seconds{namespace,policy}` | Когда политика в последний раз отработала наступивший тик. |
| `etcd_operator_snapshot_policy_last_success_timestamp_seconds{namespace,policy}` | Когда снапшот, созданный этой политикой, в последний раз завершился успешно. |
| `etcd_operator_snapshot_policy_active_snapshots{namespace,policy}` | Сколько созданных ею снапшотов ещё не закончились. |

Расхождение между `last_schedule` и `last_success` — это отдельный сигнал: снапшоты штампуются и падают. Политика при этом выглядит работающей.

### Готовые правила

```yaml
groups:
  - name: etcd-operator
    rules:
      # Кластер не собрался. Поды могут при этом работать.
      - alert: EtcdClusterNotBootstrapped
        expr: etcd_operator_cluster_bootstrapped == 0
        for: 15m

      # Потерян кворум: готовых членов меньше floor(n/2)+1.
      - alert: EtcdClusterQuorumLost
        expr: >
          etcd_operator_cluster_members_ready
            < floor(etcd_operator_cluster_members_desired / 2) + 1
        for: 2m

      # Деградация: кворум есть, но состав неполный.
      - alert: EtcdClusterDegraded
        expr: >
          etcd_operator_cluster_members_ready
            < etcd_operator_cluster_members_desired
          and
          etcd_operator_cluster_members_ready
            >= floor(etcd_operator_cluster_members_desired / 2) + 1
        for: 15m

      # Бэкап устарел.
      - alert: EtcdBackupStale
        expr: etcd_operator_snapshot_last_success_age_seconds > 24 * 3600
        for: 30m

      # Бэкапа не было НИ РАЗУ. Отдельное правило, потому что при отсутствии
      # серии предыдущее выражение не сработает.
      - alert: EtcdNeverBackedUp
        expr: >
          etcd_operator_cluster_info
            unless on(namespace, cluster)
          etcd_operator_snapshot_last_success_age_seconds
        for: 1h

      # У кластера вообще нет политики бэкапов.
      - alert: EtcdClusterWithoutBackupPolicy
        expr: >
          etcd_operator_cluster_info
            unless on(namespace, cluster)
          etcd_operator_snapshot_policy_info
        for: 1h

      # Политика штампует снапшоты, и они падают.
      - alert: EtcdBackupPolicyFailing
        expr: >
          etcd_operator_snapshot_policy_last_schedule_timestamp_seconds
            - etcd_operator_snapshot_policy_last_success_timestamp_seconds
            > 12 * 3600
        for: 30m

      # Политику приостановили и забыли вернуть.
      - alert: EtcdBackupPolicySuspended
        expr: etcd_operator_snapshot_policy_suspended == 1
        for: 24h

      # Два члена одного кластера оказались на одном массиве: потеря этого
      # массива стоит больше одного члена. Ловит перекос раскладки по пулам,
      # который сам оператор сообщает только событием при создании члена.
      - alert: EtcdMembersConcentratedOnOneArray
        expr: >
          count by (namespace, cluster, storage_pool)
            (etcd_operator_member_ready) > 1
        for: 15m
```

### Полезные запросы

```promql
# Обзор по всем дочерним кластерам: готово / нужно.
etcd_operator_cluster_members_ready / etcd_operator_cluster_members_desired

# Сколько кластеров сейчас деградировали.
count(etcd_operator_cluster_members_ready < etcd_operator_cluster_members_desired)

# Самые старые бэкапы — что чинить первым.
topk(10, etcd_operator_snapshot_last_success_age_seconds)

# Почему кластер недоступен: причина прямо в метке.
etcd_operator_cluster_condition{condition="Available"} == 0

# Раскладка кластера по массивам.
etcd_operator_member_ready{cluster="tenant-a-etcd"}

# Размер бэкапа схлопнулся — бэкапы идут, но ничего не захватывают.
etcd_operator_snapshot_last_success_size_bytes
  < 0.5 * avg_over_time(etcd_operator_snapshot_last_success_size_bytes[7d])
```

### Число серий

На родительском кластере со 160 трёхчленными кластерами это примерно 3000 серий: по 5 на кластер, по 2 на члена, по 3–4 на кластер для бэкапов и по 5 на политику. Метка `reason` у условий меняется вместе с состоянием кластера, но мёртвых серий не накапливает — при чтении в момент скрейпа существует только текущая.

## Наблюдение за задержками хранилища

Когда все data-dir лежат на массиве хранения, задержка fsync этого массива оказывается на критическом пути etcd. Порты метрик обоих членов (`:2381`, всегда открытым текстом, всегда включены) отдают две важные гистограммы:

| Метрика | Что означает | Ориентировочный порог |
|---|---|---|
| `etcd_disk_wal_fsync_duration_seconds` | Сколько занимает fsync WAL. Это путь записи каждой зафиксированной записи. | p99 > 25 мс — обычная граница «это хранилище слишком медленное для etcd». |
| `etcd_disk_backend_commit_duration_seconds` | Сколько занимает коммит бэкенда (bbolt). | p99 > 25 мс; стабильно высокое значение при нормальном fsync указывает на настройки пакетирования бэкенда. |
| `etcd_server_leader_changes_seen_total` | Смены лидера. | Любая устойчивая скорость без отказов нод означает, что тайминги консенсуса слишком тесны для этого хранилища — см. ниже. |

Растущая частота смен лидера на здоровом кластере — это подпись того, что умолчания таймингов консенсуса etcd встретились с сетевым хранилищем. Лечится через `spec.options.heartbeatIntervalMilliseconds` и `electionTimeoutMilliseconds` (см. [тюнинг под сетевое хранилище](concepts.md#тюнинг-под-сетевое-хранилище)); типичные значения на массиве — 250–500 мс и 1250–2500 мс соответственно. Учтите, что они применяются **только к вновь создаваемым членам** — существующие поды оператор не перекатывает, — поэтому после изменения удаляйте поды по одному, чтобы пересобрать кластер по новому шаблону.

## Восстановление кластера из снапшота

Восстановление возможно **только при первом бутстрапе**: *новый* кластер инициализирует data-dir своего сида из снапшота вместо пустого старта. Восстановиться в существующий, уже забутстрапленный кластер нельзя (`spec.bootstrap` неизменяем после создания) — удаляйте и создавайте заново.

> **Восстановление пересобирает data-dir тем `etcdutl`, что соответствует версии.** Агент восстановления запускает `etcdutl`, вложенный в целевой образ etcd (`v<spec.version>`), — ту самую версию, которая затем и стартует на пересобранном data-dir, — поэтому формат на диске у `etcdutl` и у etcd совпадает по построению для любой поддерживаемой оператором версии, и совпадения `spec.version` со сборкой самого оператора не требуется.
>
> Это гарантирует связку `etcdutl`↔etcd, но **не** снапшот↔etcd: версия происхождения снапшота нигде не записывается и не проверяется. Восстановление снапшота от **более новой** etcd в **более старую** `spec.version` (например, снапшот 3.6 в кластер `3.5.x`) запускает более старый `etcdutl` над базой, написанной более новым минором, — это не проверено и не поддерживается. Восстанавливайтесь в `spec.version` не ниже версии источника снапшота.

> **⚠️ Восстановление снапшота из кластера с аутентификацией.** Снапшот etcd захватывает хранилище данных *вместе с состоянием аутентификации* — пользователями, ролями и флагом включённости. Снапшот, снятый при включённой аутентификации, восстанавливается в кластер, где **etcd стартует с уже включённой аутентификацией**. Поэтому на новом `EtcdCluster` нужно выставить соответствующий `spec.auth`, иначе оператор не сможет им управлять:
>
> - Задайте `spec.auth.enabled: true` и `spec.auth.rootCredentialsSecretRef` (что дополнительно требует `spec.tls.client` — те же правила CEL, что и для любого кластера с аутентификацией; см. [Аутентификация](concepts.md#аутентификация)).
> - `password` в указанном Secret **обязан совпадать с паролем root, действовавшим на момент снятия снапшота**: etcd хранит bcrypt-хэш пароля в самом хранилище, поэтому свежий или случайный пароль против восстановленного хэша не пройдёт. Переиспользуйте исходный Secret с учётными данными, если он у вас сохранился.
>
> Если `spec.auth` опущен (как в примере ниже — там восстанавливается снапшот *без* аутентификации), восстановленная etcd с включённой аутентификацией отклонит анонимные обращения оператора. Оператор это распознаёт и выставляет `Available=False` и `Degraded=True` с причиной **`AuthRequiredNotConfigured`** и внятным сообщением — он **не** зацикливается молча. Поскольку аутентификация неизменяема после создания, лечится это удалением и созданием заново с заданным `spec.auth` (автоопределения состояния аутентификации в снапшоте при восстановлении нет; этот контракт на вас). Когда `spec.auth` задан правильно, оператор обнаруживает, что аутентификация уже включена (через `AuthStatus`), и фиксирует `status.authEnabled`, не переустанавливая пользователя root.

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: etcd-operator.cozystack.io/v1alpha2
kind: EtcdCluster
metadata:
  name: my-etcd          # можно переиспользовать старое имя, когда старый кластер ушёл
  namespace: <ns>
spec:
  replicas: 3
  version: 3.6.11          # восстановление пересоберёт через etcdutl этой версии
  storage:
    size: 1Gi
  bootstrap:
    restore:
      source:
        s3:
          endpoint: https://s3.example.com
          bucket: etcd-snapshots
          key: my-etcd/my-etcd-2026-06-02.db   # ТОЧНЫЙ ключ объекта (не префикс)
          region: us-east-1
          forcePathStyle: true
          credentialsSecretRef:
            name: s3-creds
EOF

# Под сида запускает два init-контейнера до старта etcd, именно в этом порядке:
# "install-tools" раскладывает бинарь агента, затем "restore" пересобирает data-dir.
kubectl get etcdcluster.etcd-operator.cozystack.io my-etcd -n <ns> -w
kubectl logs -n <ns> <под-сида> -c restore
# Если "restore" так и не стартовал, не удалась раскладка — смотрите туда:
kubectl logs -n <ns> <под-сида> -c install-tools
```

Замечания:

- Для **источника** восстановления локатор точен: `s3.key` — это полный ключ объекта (то, что сообщил `status.artifact.uri`, без префикса `s3://bucket/`), а для источника на PVC `pvc.subPath` — полный путь к файлу `.db` внутри тома.
- Агент восстановления пересобирает data-dir через `etcdutl`, используя идентичность сида (имя, initial-cluster, токен, peer URL), после чего etcd стартует на нём. Члены, добавляемые масштабированием позже, присоединяются к живому кластеру обычным порядком — восстанавливается только сид.
- Восстановление идемпотентно: как только data-dir инициализирован, init-контейнер ничего не делает, поэтому перезапуски пода после первого старта никогда не перезагружают снапшот и не затирают живые данные.
- После восстановления etcd назначает **новый** cluster ID — это свежий кластер, засеянный старыми данными, а не продолжение прежнего.
- **Рассчитывайте том данных примерно на 2 размера снапшота на время восстановления.** Агент размещает и снапшот, и пересобираемый data-dir на томе данных (загрузка из S3 тоже идёт туда, а не в эфемерный `/tmp` контейнера), поэтому кратковременно занимает примерно вдвое больше размера снапшота. Агент выполняет предварительную проверку свободного места и падает заранее с внятным сообщением, если `spec.storage.size` мал, — увеличьте размер до повтора, а не позволяйте восстановлению умереть на середине. Для источника в S3 размер узнаётся через `HeadObject` **до** начала загрузки, поэтому недостаточный том падает на проверке, а не на ENOSPC посреди передачи. (Источник на PVC читается с отдельного тома, смонтированного только на чтение, поэтому место в data-dir расходует только пересборка.)

### Проверяйте снапшот, из которого восстанавливаетесь

Задайте `bootstrap.restore.checksum` той контрольной суммой, которую записал снапшот:

```yaml
  bootstrap:
    restore:
      checksum: "sha256:9f2b..."     # status.artifact.checksum, дословно
      source:
        s3: {...}
```

Делайте это для любого восстановления, которое имеет значение. Снапшот, снятый через API `Maintenance.Snapshot` в etcd, не несёт собственного хэша, поэтому `etcdutl snapshot restore` запускается с `--skip-hash-check`, и без этого поля **на всём пути ничего ничего не проверяет**. Усечённая загрузка или повреждённый объект дают каталог данных, на котором etcd спокойно стартует; кластер сообщает, что здоров, а повреждение всплывает позже, когда кто-нибудь прочитает недостающую часть.

Контрольная сумма лежит в `status.artifact.checksum` у `EtcdSnapshot` либо в `status.lastSuccessfulArtifact.checksum` у `EtcdSnapshotPolicy`. Отсутствие поля сохраняет прежнее поведение (без проверки); а некорректное значение роняет восстановление, а не пропускается молча, поэтому опечатка не может тихо отключить проверку.

## Аварийное восстановление дочернего кластера на месте

Порядок действий, когда кластер невосстановим — потерян кворум при нескольких сломанных голосующих либо повреждены данные — и вы поднимаете его под тем же именем, чтобы всё, что к нему подключается, не пришлось перенастраивать.

Ничего из этого не обратимо. Прочитайте до конца перед началом; разрушительный шаг — четвёртый.

**1. Остановите тех, кто пишет.** Масштабируйте в ноль то, что использует этот etcd. Для control plane дочернего кластера в Kamaji:

```sh
kubectl patch tenantcontrolplane <tcp> -n <ns> --type=merge \
  -p '{"spec":{"controlPlane":{"deployment":{"replicas":0}}}}'
```

Восстановление, выполняемое пока apiserver ещё пишет, — это восстановление движущейся мишени. Этот шаг ещё и делает следующий честным: та ревизия, к которой вы восстановитесь, будет последней, которую кто-либо записал.

**2. Выберите снапшот и запишите его контрольную сумму.**

```sh
kubectl get etcdsnapshotpolicy <policy> -n <ns> -o jsonpath='{.status.lastSuccessfulArtifact}{"\n"}'
# либо по отдельным снапшотам:
kubectl get etcdsnapshot -n <ns> -l etcd-operator.cozystack.io/cluster=<cluster>
```

Если кластер ещё доступен, а самый свежий снапшот старше приемлемого, **снимите новый прямо сейчас** — до того, как шаг 4 лишит вас этой возможности. Деградировавший кластер, у которого ещё есть кворум, обычно снять снапшот позволяет.

**3. Сохраните учётные данные.** Если у кластера есть `spec.auth`, указанный Secret обязан пережить удаление: etcd хранит *bcrypt-хэш* пароля root внутри снапшота, поэтому восстановленный кластер примет только тот пароль, что действовал на момент снятия. Свежесгенерированный не пройдёт, а аутентификация неизменяема после создания — ошибка не лечится ничем, кроме ещё одного восстановления.

```sh
kubectl get secret <root-creds> -n <ns> -o yaml > /tmp/root-creds.yaml
```

То же сделайте с Secret TLS, если ими не управляет cert-manager.

**4. Удалите кластер.** PVC членов уходят вместе с ним; на этот момент снапшот — единственная копия.

```sh
kubectl delete etcdcluster <cluster> -n <ns>
kubectl get pvc -n <ns> -l etcd-operator.cozystack.io/cluster=<cluster>   # ожидается пусто
```

**5. Создайте заново с тем же именем и источником восстановления.** Имя то же, чтобы клиентский Service — а значит, и строка подключения, которую держит control plane дочернего кластера, — не изменился. Перенесите `spec.auth`, `spec.tls` и, если кластер их использовал, те же `spec.storage.pools` в том же порядке: сид детерминированно попадает в `pools[0]`, и именно это делает процедуру воспроизводимой.

`spec.version` обязана быть **не ниже** версии, из которой снят снапшот. Пересборка использует `etcdutl` из целевого образа etcd, поэтому `etcdutl` и etcd всегда согласованы, — но версия происхождения самого снапшота нигде не записана, и восстановление более нового снапшота в более старую etcd не проверено.

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: etcd-operator.cozystack.io/v1alpha2
kind: EtcdCluster
metadata:
  name: <cluster>            # ТО ЖЕ имя
  namespace: <ns>
spec:
  replicas: 3
  version: 3.6.11
  storage:
    size: 20Gi
    pools:                   # те же пулы, в том же порядке
      - {name: array-a, storageClassName: san-a}
      - {name: array-b, storageClassName: san-b}
      - {name: array-c, storageClassName: san-c}
  bootstrap:
    restore:
      checksum: "sha256:9f2b..."
      source:
        s3:
          endpoint: https://s3.example.com
          bucket: etcd-snapshots
          key: my-etcd/my-etcd-backup-29558640.db   # ТОЧНЫЙ ключ объекта
          region: us-east-1
          forcePathStyle: true
          credentialsSecretRef:
            name: s3-creds
EOF
```

**6. Понаблюдайте за восстановлением сида, затем за сборкой кластера.**

```sh
kubectl get etcdcluster <cluster> -n <ns> -w
kubectl logs -n <ns> <под-сида> -c restore        # пересборка
kubectl logs -n <ns> <под-сида> -c install-tools  # если "restore" не стартовал
```

Дождитесь `Available=True` со всеми готовыми репликами. Затем убедитесь, что восстановили именно то, что хотели, — ревизия должна быть близка к ревизии снапшота:

```sh
kubectl etcd endpoint status --cluster <cluster> -n <ns>
```

**7. Верните пишущих.** Масштабируйте control plane дочернего кластера обратно и проверьте, что она поднимается на восстановленных данных.

### Чего ждать после

- **Cluster ID новый.** Это свежий кластер, засеянный старыми данными, а не продолжение прежнего. Всё, что кэшировало прежние cluster ID или member ID, следует считать устаревшим.
- **Всё записанное после снапшота потеряно.** Это окно ровно равно интервалу вашей `EtcdSnapshotPolicy` — довод в пользу короткого интервала.
- **Дальше восстановление идемпотентно.** Как только data-dir инициализирован, init-контейнер ничего не делает, поэтому перезапуски пода сида не перезагружают снапшот и не затирают живые данные.
- **Верните политику бэкапов**, если она была привязана к удалённому кластеру: `EtcdSnapshotPolicy`, ссылающаяся на него по имени, продолжит работать, раз вы переиспользовали имя, но убедитесь, что `status.lastSuccessfulTime` снова начал продвигаться.

## Чтение состояния etcd напрямую

Оператор показывает только то, что нужно ему самому для решений. Для более глубокого разбора говорите с etcd:

```sh
POD=$(kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns> \
  -o jsonpath='{.items[0].metadata.name}')

# Cluster ID, лидер, члены:
kubectl exec -n <ns> "$POD" -- etcdctl --endpoints=http://localhost:2379 \
  member list -w table
kubectl exec -n <ns> "$POD" -- etcdctl --endpoints=http://localhost:2379 \
  endpoint status -w table --cluster

# Здоровье по каждому члену:
kubectl exec -n <ns> "$POD" -- etcdctl --endpoints=http://localhost:2379 \
  endpoint health --cluster

# Размер базы, последняя ревизия, срок raft:
kubectl exec -n <ns> "$POD" -- etcdctl --endpoints=http://localhost:2379 \
  endpoint status --cluster -w json | jq
```

`IS LEARNER=true` в `member list` указывает на члена, которого ещё не повысили, — это ожидаемо на шаге масштабирования вверх и ненормально в установившемся режиме. `promotePendingLearner` оператора запускается и из `scaleUp`, и из ветки `current == desired` в `Reconcile`, поэтому learner, остающийся learner дольше нескольких циклов reconcile, либо имеет проблему с синхронизацией (смотрите логи пода etcd), либо оператор заклинило (смотрите логи пода оператора).

## Логи оператора

Оператор по умолчанию работает в `etcd-operator-system`. Строки логов, которые встречаются чаще всего:

```sh
kubectl logs -n etcd-operator-system deploy/etcd-operator \
  -c manager --tail=200
```

Ключевые сигналы:

| Сообщение в логе | Что означает |
|---|---|
| `bootstrapping single-node cluster` | Бутстрап на первом reconcile создаёт сид. |
| `waiting for bootstrap member to form cluster` | Сид создан, ждём его под и обнаружение через etcd. |
| `cluster declared paused from the start; not bootstrapping` | `spec.replicas=0` на кластере, который никогда не бутстрапился. |
| `completing pending scale-up member before further action` | Восстановление после сбоя: предыдущее reconcile оставило CR с пустым `spec.initialCluster`. |
| `added member as learner` | `MemberAddAsLearner` прошёл; следующий reconcile пропатчит `spec.initialCluster`. |
| `promoted learner` | `MemberPromote` прошёл; член стал голосующим. |
| `waking dormant member` | Возобновление: патчится `spec.dormant=false`. |
| `waiting for existing members to become Ready before next scale-up step` | Масштабирование идёт по одному шагу; предыдущий learner ещё не готов. |
| `learner not yet promotable; will retry` | `MemberPromote` вернул ошибку «in sync with leader»; при масштабировании это безобидно. |
| `MemberList failed` (ERROR) с `rpc not supported for learner` | Фильтрация эндпоинтов (исправление issue #12) должна это предотвращать; если видите — заводите баг. |

## Кластеры в памяти

Включаются явно через `spec.storage.medium: Memory`. Data-dir каждого члена — это tmpfs `emptyDir`, живущий ровно столько, сколько под. Подходит только для восстановимых нагрузок — модель и компромиссы см. в [концепциях](concepts.md#хранилище).

### Создать кластер в памяти

```sh
cat <<'EOF' | kubectl apply -f -
apiVersion: etcd-operator.cozystack.io/v1alpha2
kind: EtcdCluster
metadata:
  name: my-mem-etcd
  namespace: default
spec:
  replicas: 3
  version: 3.6.11
  storage:
    size: 256Mi
    medium: Memory
EOF
```

`storage.size` теперь задаёт `SizeLimit` tmpfs, а не ёмкость PVC. Берите с запасом: WAL etcd плюс сам набор ключей плюс буфер под компакцию. 256Mi хватает для наборов меньше мегабайта; для чего-либо нагруженного увеличивайте.

### Убедиться, что это действительно tmpfs

Том пода говорит сам за себя:

```sh
kubectl get pod -l etcd-operator.cozystack.io/cluster=my-mem-etcd -n default \
  -o jsonpath='{.items[0].spec.volumes[?(@.name=="data")]}' | jq
# Ожидается: {"emptyDir": {"medium": "Memory", "sizeLimit": "256Mi"}, ...}
```

И изнутри пода:

```sh
POD=$(kubectl get pod -l etcd-operator.cozystack.io/cluster=my-mem-etcd -n default \
  -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n default "$POD" -- mount | grep /var/lib/etcd
# Ожидается: tmpfs on /var/lib/etcd type tmpfs (...)
```

PVC у кластера быть не должно:

```sh
kubectl get pvc -l etcd-operator.cozystack.io/cluster=my-mem-etcd -n default
# No resources found.
```

### Что настроить перед продакшеном

`PodDisruptionBudget` теперь выпускается автоматически (повседневную картину см. в [Вытеснение с нод](#вытеснение-с-нод-poddisruptionbudget)). Ни одно из следующих двух не проставляется по умолчанию (отслеживается в [#16](https://github.com/lllamnyp/etcd-operator/issues/16)), поэтому задайте оба явно:

1. **Anti-affinity подов** — задайте `spec.affinity` на кластере (передаётся каждому поду члена; см. [концепции: планирование подов](concepts.md#планирование-подов-и-дополнительные-метаданные)):

   ```yaml
   spec:
     affinity:
       podAntiAffinity:
         requiredDuringSchedulingIgnoredDuringExecution:
           - labelSelector:
               matchLabels:
                 etcd-operator.cozystack.io/cluster: my-mem-etcd
             topologyKey: kubernetes.io/hostname
   ```

   Доступен и `spec.topologySpreadConstraints` для разноса по зонам и нодам, а `spec.priorityClassName` не даёт членам выселяться с ноды под давлением первыми (см. [концепции](concepts.md#specpriorityclassname)). Все три действуют на вновь создаваемых членов; чтобы применить изменение, перекатывайте существующие поды по одному.

2. **Лимит памяти контейнера** — задайте `spec.resources.limits.memory` на кластере, чтобы записи в tmpfs учитывались в cgroup пода, а не в памяти ноды:

   ```yaml
   spec:
     storage:
       size: 256Mi
       medium: Memory
     resources:
       requests:
         memory: 384Mi
       limits:
         memory: 512Mi   # >= spec.storage.size + ~128Mi запаса для etcd
   ```

   Без лимита записи в tmpfs учитываются в памяти ноды, а не в cgroup пода, под работает в классе BestEffort или Burstable и первым идёт на вытеснение под давлением — это ровно тот отказ, который уничтожает членов в памяти. Изменения `spec.resources` действуют на вновь создаваемых членов; существующие поды сохраняют исходные размеры, пока их не перекатят.

### Потеря пода и автозамена

Оператор обнаруживает потерю пода через `Status.PodUID`. Два сценария:

- **Потерян один под, кворум держится**: оператор удаляет CR EtcdMember, финализатор выполняет `MemberRemove` у соседей, масштабирование в контроллере кластера создаёт свежую замену с новым именем от `GenerateName` и новым member ID в etcd. Кластер лечится сам.
- **Одновременно потеряно больше кворума**: `MemberRemove` у уцелевших соседей не проходит (кворума нет), умирающие члены остаются в `Terminating`. Кластер мёртв, и пользователю придётся создавать его заново.

**Задержка обнаружения зависит от того, что убило под.** `kubectl delete pod` или вытеснение kubelet переводят под в NotFound за секунды — автозамена начинается на следующем reconcile (около 5 с). Уход ноды в NotReady медленнее: kubelet на здоровой ноде убрал бы всё немедленно, но выход пода из Terminating гейтится параметром `--pod-eviction-timeout` у kube-controller-manager (по умолчанию 5 минут). До этого проверка потери в операторе видит под с тем же UID (статус сообщает Terminating, но с точки зрения API он ещё существует) и ждёт — это лучше, чем гоняться со сборкой kubelet. Поэтому закладывайте **до 5 минут деградировавшего кворума** при внезапном отказе ноды, где живёт etcd. Если нужно быстрее, настраивайте `--pod-eviction-timeout` у kube-controller-manager на весь кластер; оператор на это не влияет.

Посмотреть, как происходит автозамена:

```sh
kubectl get etcdmember.etcd-operator.cozystack.io -n default -w
# Изначально: my-mem-etcd-abc12, my-mem-etcd-def34, my-mem-etcd-ghi56.
# Принудительно удалите один под: kubectl delete pod -n default my-mem-etcd-abc12
# Понаблюдайте, как CR EtcdMember удаляется, появляется новый со свежим
# именем от GenerateName, и READY=3 восстанавливается примерно за минуту.
```

### Замена члена в crash-loop

Механизм с `Status.PodUID` выше опирается на исчезновение пода. Он не видит члена, чей под *жив*, но чья etcd не может стартовать: под сохраняет UID, пока контейнер etcd внутри него крутится в crash-loop. Для этого состояния есть отдельный, второй триггер, покрывающий оба типа хранилища.

Если etcd члена не может стартовать — потому что его замороженный `--initial-cluster` устарел, пока состав кластера ушёл вперёд (`error validating peerURLs ... member count is unequal`), будь то член на PVC, потерявший data-dir (например, том потерян при отказе ноды), или learner-замена на любом носителе, чей состав изменился между созданием пода и первым удачным стартом, — он крутится в crash-loop бесконечно без собственного пути к восстановлению. Оператор это обнаруживает и заменяет его:

- **Обнаружение**: контейнер etcd не готов и перезапускался не менее 5 раз (`dataLossRestartThreshold`), исключая `OOMKilled` (это проблема ресурсов, а не потерянного data-dir; лечится поднятием `spec.resources.limits.memory`, а не заменой). Под, который удаляется (вытеснение, drain, ручной перезапуск), застрявшим никогда не считается.
- **Гейт по кворуму**: оператор удаляет члена, только когда у *остальной* части кластера сохраняется кворум, поэтому общая авария никогда не превращается в массовое удаление. Гейт читает `Status.ReadyMembers` (его ведёт контроллер кластера, и он может отставать) и вычитает застрявшего члена, если тот ещё посчитан; `MemberRemove` в финализаторе независимо гейтится кворумом как запасная защита.
- **Сид не исключение**: то, что он сформировал кластер, — факт происхождения, а не постоянная индульгенция. Защищено только *окно* бутстрапа, и делает это один лишь гейт по кворуму: до фиксации `clusterID` ничто ещё не считалось готовым, поэтому `ReadyMembers` равен 0 и не проходит ни один член. Та же арифметика постоянно защищает единственного члена кластера с одной репликой.
- **Замена**: CR `EtcdMember` удаляется → финализатор выполняет `MemberRemove` → PVC `data-<member>`, принадлежащий члену, собирается сборщиком мусора (повреждённый data-dir выбрасывается; у членов в памяти его нет) → контроллер кластера заполняет пробел свежим членом с именем от `GenerateName`, актуальным `--initial-cluster` и **новым** member ID в etcd.

**Задержка обнаружения здесь намного больше, чем на пути потери пода.** `CrashLoopBackOff` упирается в потолок в 5 минут, поэтому набрать 5 перезапусков занимает **десятки минут**, а не около 5 секунд. Закладывайте это, прежде чем заключить, что оператор ведёт себя неправильно: член, который исчезает и заменяется другим — со свежим именем — после долгого crash-loop, это оператор, работающий как задумано, а не дребезг. Учтите также, что замена, которая сама медленно поднимается (медленное восстановление, медленное присоединение learner), может перешагнуть тот же порог и быть заменённой снова; это гейтится кворумом и для кластера безвредно, но на действительно нездоровом члене ждите повторных замен.

### Пауза не поддерживается

Установка `spec.replicas: 0` на кластере в памяти **отклоняется apiserver** (правило проверки CEL на `EtcdClusterSpec`):

```
kubectl patch etcdcluster.etcd-operator.cozystack.io my-mem-etcd -n default --type=merge \
  -p '{"spec":{"replicas":0}}'
# The EtcdCluster "my-mem-etcd" is invalid: spec: Invalid value: ...:
#   spec.replicas=0 with spec.storage.medium=Memory is unsupported: ...
```

Пауза кластера в памяти заклинила бы его при возобновлении (под удалён → tmpfs исчез → путь пробуждения считает пустой data-dir сохранённым → etcd отказывается стартовать). Чтобы свернуть кластер в памяти, удалите `EtcdCluster` и создайте заново.

## Вытеснение с нод (PodDisruptionBudget)

Каждый `EtcdCluster` несёт собственный `PodDisruptionBudget`, названный по имени кластера. Полное устройство — в [концепциях](concepts.md#poddisruptionbudget); повседневная картина:

```sh
kubectl get pdb -n <ns> <cluster>
# NAME       MIN AVAILABLE   MAX UNAVAILABLE   ALLOWED DISRUPTIONS   AGE
# my-etcd    2               N/A               1                     12m
```

`MIN AVAILABLE` — это нижняя граница: кворум от предполагаемого размера кластера (или от живого числа голосующих во время уменьшения — что больше; см. [концепции](concepts.md#poddisruptionbudget)). `ALLOWED DISRUPTIONS` — сколько вытеснений голосующих ещё укладывается в бюджет прямо сейчас (равно числу здоровых голосующих минус min available). Когда оно достигает 0, `kubectl drain` любой ноды с подом голосующего блокируется:

```
error when evicting pods/"my-etcd-7xq2k" -n my-ns:
  Cannot evict pod as it would violate the pod's disruption budget.
```

Это и есть задуманное поведение — ваш drain только что отказался ломать кворум. Решается ожиданием возвращения недоступного голосующего либо пониманием того, что до этого drain должно стать готово больше нод.

### Одночленные кластеры блокируют drain навсегда

Граница равна `⌊n/2⌋ + 1` от целевого размера, поэтому **кластер с одной репликой получает `minAvailable: 1` при ровно одном поде голосующего — `ALLOWED DISRUPTIONS: 0`, навсегда.** `kubectl drain` ноды, на которой он стоит, не тормозит на временном условии; он не может завершиться никогда, пока кластер держит одну реплику.

Арифметически это верно — вытеснение единственного члена *и есть* простой, — но само по себе это состояние не разрешается, и на родительском кластере со множеством одночленных дочерних кластеров это означает, что любой drain ноды блокируют те дочерние кластеры, чьи члены на ней оказались. Прежде чем вытеснять, выясните, кто это:

```sh
# Какие члены etcd живут на ноде и какому кластеру каждый принадлежит.
kubectl get pod -A --field-selector spec.nodeName=<node> \
  -l etcd-operator.cozystack.io/role=voter \
  -o custom-columns=NS:.metadata.namespace,POD:.metadata.name,CLUSTER:.metadata.labels."etcd-operator\.cozystack\.io/cluster"

# Есть ли у каждого бюджет на вытеснение?
kubectl get pdb -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,ALLOWED:.status.disruptionsAllowed
```

Любой кластер с `ALLOWED: 0` заблокирует drain. Для одночленного кластера есть три честных варианта и никакого ловкого четвёртого:

1. **Сначала увеличить до 3** (`kubectl scale --replicas=3` либо `replicas` в чарте), вытеснить, вернуть обратно. Кластер всё это время обслуживает запросы. Это единственный вариант без простоя, и именно поэтому дочерний кластер, который важен, не должен оставаться на одной реплике во время обслуживания.
2. **Принять простой**: удалить PDB, вытеснить, дать оператору перепланировать члена. Контрольная панель дочернего кластера недоступна с момента вытеснения до момента, когда под снова запустится: PVC нужно отцепить и подцепить заново, а на сетевом хранилище это не мгновенно. PDB оператор пересоздаст на следующем reconcile.
3. **Переместить нагрузку вместо вытеснения** — `kubectl drain --pod-selector`, чтобы вытеснить всё остальное, и держать ноду под cordon, пока дочернего кластера не увеличат или не мигрируют.

Не берите `--disable-eviction` (он удаляет поды напрямую, минуя бюджет) как обычную практику: на одночленном кластере это вариант 2 без осознанного шага по снятию защиты.

### Одночленные кластеры не лечатся сами

Путь замены при crash-loop гейтится кворумом: член удаляется и заменяется, только когда у *остальной* части кластера сохраняется большинство. На кластере с одной репликой остальной части нет, поэтому гейт не проходит никогда — что верно, поскольку удаление единственного члена уничтожает вместе с ним и данные, — но это значит, что оператор его не починит.

Одночленный кластер, чей каталог данных потерян или повреждён, остаётся в `Available=False` с подом в crash-loop, пока не вмешается человек, и единственное действие, которое его возвращает, — это [восстановление из снапшота](#аварийное-восстановление-дочернего-кластера-на-месте).

Практическое следствие: для одночленного кластера [`EtcdSnapshotPolicy`](#регулярные-бэкапы) — не приятное дополнение. Это механизм восстановления. Трёхчленный кластер может потерять члена и пересобрать его от соседей; у одночленного соседей нет, и его бэкап — единственная копия данных.

Та же асимметрия действует и для дефрагментации: `Defragment` останавливает мир на том члене, где он выполняется, а на одночленном кластере передать лидерство некому, поэтому останавливается весь кластер на всё время операции. Планируйте запуски [`EtcdDefragPolicy`](etcd-defrag.md) для одночленных дочерних кластеров в окно, где пауза допустима.

### Какие поды голосующие

Поды голосующих несут метку `etcd-operator.cozystack.io/role=voter`. У learner её нет. Найти их:

```sh
kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster>,etcd-operator.cozystack.io/role=voter -n <ns>
```

Сверьте с `kubectl get etcdmember.etcd-operator.cozystack.io -n <ns>` — у голосующих там `Status.IsVoter: true` (его пишет контроллер кластера по `MemberList` из etcd).

### Почему drain может блокироваться во время масштабирования

PDB обновляется **на одно reconcile позже**, чем меняется картина в etcd (следующий проход контроллера кластера подхватывает новое число голосующих из `MemberList`). Это сделано намеренно — разбор безопасности см. в [концепциях](concepts.md#переходные-гонки). Окно гонки шириной в один цикл reconcile (в установившемся режиме `RequeueAfter` равен 30 с, то есть в худшем случае до ~30 с); drain, предпринятый в этом окне, падает закрыто (отказывает в вытеснении), а не открыто, и это верное направление.

Отдельно: граница привязана к *предполагаемому* размеру кластера, а не только к живому числу голосующих. Пока кластер ниже цели — бутстрап, увеличение или ротация нод, убравшая членов быстрее, чем оператор успел восполнить, — граница остаётся на кворуме цели даже при падении числа здоровых голосующих, поэтому бюджет ужимается с каждым недостающим и достигает нуля, как только здоровых голосующих становится `⌊цель/2⌋ + 1`. На цели в 3 члена это любая нехватка вообще; на больших целях нужна нехватка больше одного (цель 5 при 4 здоровых голосующих всё ещё разрешает 1 вытеснение, цель 7 при 6 разрешает 2). Drain, застрявший здесь, — это работающий бюджет; дождитесь, пока оператор восстановит недостающих голосующих.

Если вы делаете плановое поочерёдное обслуживание нод, сначала уменьшите кластер до устойчивого размера кворума, вытесняйте, затем увеличьте обратно.

## Рецепты

### Найти спящего члена

```sh
kubectl get etcdmember.etcd-operator.cozystack.io -n <ns> \
  -o jsonpath='{range .items[?(@.spec.dormant==true)]}{.metadata.name}{"\n"}{end}'
```

### Найти сид работающего кластера

```sh
kubectl get etcdmember.etcd-operator.cozystack.io -n <ns> \
  -o jsonpath='{range .items[?(@.spec.bootstrap==true)]}{.metadata.name}{"\n"}{end}'
```

Замечание: после бутстрапа у сида нет особой эксплуатационной роли — см. [концепции](concepts.md#именование-членов). Это для исторической справки, а не для маршрутизации.

### Смотреть логи лидера etcd

```sh
POD=$(kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns> \
  -o jsonpath='{.items[0].metadata.name}')
LEADER=$(kubectl exec -n <ns> "$POD" -- etcdctl \
  --endpoints=http://localhost:2379 endpoint status --cluster -w json \
  | jq -r '.[] | select(.Status.leader == .Status.header.member_id) | .Endpoint' \
  | sed 's|http://||;s|:.*||;s|\..*||')
kubectl logs -n <ns> "$LEADER" --tail=200
```

### Вытеснить ноду, на которой стоит член etcd

Обычных `kubectl cordon` и `kubectl drain` достаточно. Под вытесняется, перепланируется на другую ноду, PVC подцепляется заново (если storage class поддерживает перемещение) или остаётся на месте (если нет — тогда под будет в Pending, пока нода не вернётся). Etcd это переживает нормально: `MemberAddAsLearner` тут не задействован, потому что member ID и data-dir сохранены в PVC.

PodAntiAffinity по умолчанию не настроена. Два пода etcd могут оказаться на одной ноде, а значит, один drain способен разом убрать двух голосующих и потерять кворум на трёхчленном кластере. Задайте `spec.affinity` (и/или `spec.topologySpreadConstraints`) на кластере, чтобы держать голосующих порознь, — см. [концепции: планирование подов](concepts.md#планирование-подов-и-дополнительные-метаданные) и [чеклист перед продакшеном](#что-настроить-перед-продакшеном).

## Кластеры с TLS

Два пути к кластеру с TLS, взаимоисключающие в пределах поддерева:

- **Свои Secret** — вы создаёте Secret вне оператора и ссылаетесь на них из `spec.tls.{client,peer}.{serverSecretRef,operatorClientSecretRef,secretRef}`. Об этом раздел ниже.
- **cert-manager** — направьте `spec.tls.{client,peer}.certManager` на Issuer или ClusterIssuer, оператор выпустит ресурсы `cert-manager.io/v1 Certificate`, а cert-manager создаст Secret. Переходите к [кластерам под управлением cert-manager](#кластеры-под-управлением-cert-manager).

О том, что должен содержать каждый Secret, и об ограничениях по EKU и CA-бандлу см. [концепции: TLS](concepts.md#tls).

### Создание Secret

Типичному кластеру с полным mTLS нужны три Secret в неймспейсе кластера:

```sh
# Серверный сертификат с ключом плюс CA-бандл, который заодно служит бандлом
# доверия для клиента. SAN серверного сертификата ОБЯЗАНЫ покрывать:
#   *.<cluster>.<ns>.svc,
#   *.<cluster>.<ns>.svc.<cluster-domain>,
#   <cluster>.<ns>.svc, <cluster>-client.<ns>.svc,
#   localhost (DNS SAN), 127.0.0.1 (IP SAN).
# Сдвоенные подстановки не избыточны: вторая покрывает полностью
# квалифицированную PTR-запись, которую kube-dns возвращает для IP подов и
# которую ищет проверяющий peer-mTLS в etcd. <cluster-domain> по умолчанию
# cluster.local; Cozystack и некоторые другие используют cozy.local. Как его
# определить, см. installation.md.
# EKU серверного сертификата ОБЯЗАН включать и serverAuth, и clientAuth.
kubectl create secret generic my-cluster-server-tls -n <ns> \
  --from-file=tls.crt=server.crt \
  --from-file=tls.key=server.key \
  --from-file=ca.crt=ca.crt

# Клиентская идентичность оператора для etcd. EKU ОБЯЗАН включать clientAuth.
# Подписан CA, чей открытый сертификат есть в бандле доверия выше (обычно тем
# же самым CA, и тогда бандл — это один этот сертификат).
kubectl create secret generic my-cluster-operator-client-tls -n <ns> \
  --from-file=tls.crt=op-client.crt \
  --from-file=tls.key=op-client.key

# Peer-сертификат с ключом и CA. SAN ОБЯЗАНЫ покрывать и
# *.<cluster>.<ns>.svc, И *.<cluster>.<ns>.svc.<cluster-domain> — вторая
# покрывает полностью квалифицированную PTR-запись, с которой проверяющий
# peer-mTLS в etcd сверяет обратное разрешение подключающегося узла. EKU
# peer-сертификата ОБЯЗАН включать и serverAuth, и clientAuth (peer симметричен).
kubectl create secret generic my-cluster-peer-tls -n <ns> \
  --from-file=tls.crt=peer.crt \
  --from-file=tls.key=peer.key \
  --from-file=ca.crt=peer-ca.crt
```

Затем сошлитесь на них в спецификации кластера:

```yaml
apiVersion: etcd-operator.cozystack.io/v1alpha2
kind: EtcdCluster
metadata:
  name: my-cluster
spec:
  replicas: 3
  version: 3.6.11
  storage:
    size: 1Gi
  tls:
    client:
      serverSecretRef:
        name: my-cluster-server-tls
      operatorClientSecretRef:
        name: my-cluster-operator-client-tls
    peer:
      secretRef:
        name: my-cluster-peer-tls
```

Только серверный TLS (шифрование без клиентской идентичности) — убрать строку `operatorClientSecretRef`. Открытый peer — убрать весь блок `peer:`. Оба plane включаются независимо.

### Обращение к кластеру с TLS из `etcdctl`

Зашитые в примерах выше `--endpoints=http://localhost:2379` превращаются в:

```sh
POD=$(kubectl get pod -l etcd-operator.cozystack.io/cluster=<cluster> -n <ns> \
  -o jsonpath='{.items[0].metadata.name}')

# Разложите клиентский сертификат с ключом внутри пода для etcdctl, вызываемого
# через exec. Подойдёт любой клиентский сертификат, подписанный CA из
# /etc/etcd/tls/client/ca.crt; клиентский сертификат оператора удобен тем, что
# уже выпущен. `kubectl cp` берёт локальный путь-источник (каталог, где лежат
# tls.crt и tls.key) и пишет в под по указанному пути.
kubectl cp ./client-cert-dir "<ns>/$POD:/tmp/cli"

kubectl exec -n <ns> "$POD" -- etcdctl \
  --endpoints=https://localhost:2379 \
  --cacert=/etc/etcd/tls/client/ca.crt \
  --cert=/tmp/cli/tls.crt \
  --key=/tmp/cli/tls.key \
  member list -w table
```

Для кластеров только с серверным TLS уберите `--cert` и `--key`.

### Кластеры под управлением cert-manager

Когда задан `spec.tls.{client,peer}.certManager`, оператор при reconcile выпускает три ресурса `cert-manager.io/v1 Certificate` на кластер (серверный, необязательный клиентский оператора и peer). Дальше cert-manager создаёт соответствующие Secret. Смотреть на них можно как на любые ресурсы cert-manager:

```sh
kubectl get certificate -n <ns> -l app.kubernetes.io/instance=<cluster>
kubectl describe certificate -n <ns> <cluster>-server
```

Certificate, застрявший в `Ready=False`, — самый частый вид отказа. `kubectl describe certificate <cluster>-server` покажет ошибку со стороны `Issuer` в cert-manager (нет подходящего `Issuer`, недоступен подписывающий бэкенд, истёк сертификат CA и так далее). Если проба оператора при старте не нашла cert-manager, статус EtcdCluster будет читаться как `Available=False / CertManagerNotInstalled`, и никакие Certificate созданы не будут — установите cert-manager, затем перезапустите оператора (проба обнаружения выполняется при каждом его старте).

Расхождения по `--cluster-domain` происходят молча, а проявляются как «второй член кластера уходит в crashloop с `discovery failed` или `EOF`». Оператор при старте определяет свой DNS-суффикс по строке `search` в `/etc/resolv.conf`, поэтому для обычных развёртываний в подах кластера — включая `cozy.local` у Cozystack — флаг не нужен. Если ваш оператор работает с `hostNetwork: true` или нестандартным `dnsPolicy`, автоопределение ничего не вернёт и оператор откатится на `cluster.local`; в этом случае задайте `--cluster-domain` явно.

### Ротация сертификатов

`crypto/tls` в Go часть файлов перечитывает на каждое рукопожатие, а часть запекает в `*x509.CertPool` при построении конфигурации. Порядок ротации зависит от того, какой материал изменился; для кластеров под управлением cert-manager он сам обновляет Certificate (содержимое Secret меняется), и ручной перезапуск остаётся нужен только для бандла доверия на стороне подов.

| Материал | Перечитывается на лету? | Порядок ротации |
|---|---|---|
| `tls.crt` и `tls.key` в `serverSecretRef` (свои) либо в `<cluster>-server-tls` (cert-manager) — это `--cert-file` и `--key-file` у etcd | **Да.** Заведено через `tls.Config.GetCertificate`; etcd перечитывает файлы на каждом новом рукопожатии TLS. | Свои: обновите Secret. cert-manager: ничего — обновлением занимается он сам. Новые соединения подхватывают новый сертификат в пределах цикла обновления kubelet (около 60 с на проекцию смонтированного тома). Существующие keepalive-соединения продолжают со старым сертификатом, пока не переустановятся естественным образом. |
| `tls.crt` и `tls.key` в peer-Secret (`--peer-cert-file` и `--peer-key-file`) | **Да**, тем же механизмом. | Так же, как выше. |
| `ca.crt` в серверном Secret (`--trusted-ca-file` у etcd) | **Нет.** Загружается в `x509.CertPool` при старте etcd и держится в памяти. | Обновите бандл доверия (свои: отредактируйте Secret; cert-manager: сротируйте CA у Issuer), затем перезапустите поды по одному (`kubectl delete pod <имя-пода-члена>`; дождитесь `Ready` и подтверждения возвращения через `member list`, прежде чем продолжать). |
| `ca.crt` в peer-Secret (`--peer-trusted-ca-file` у etcd) | **Нет**, по той же причине. | Так же, как выше. |
| `tls.crt`, `tls.key` и `ca.crt` в клиентском Secret оператора (на стороне оператора, в поды etcd не монтируется) | **Да** — оператор пересобирает свой `*tls.Config` из Secret на каждом reconcile, поэтому все три ротируются без перезапуска оператора. | Обновите Secret (свои) или дайте cert-manager обновить (cert-manager). Изменение вступает в силу на следующем reconcile (около 30 с в покое, немедленно при событийном срабатывании). |

Если сомневаетесь: сертификаты и ключи меняются на лету; бандлы доверия (тот самый `ca.crt`, используемый *как якорь доверия*) требуют перезапуска пода. В будущей версии оператор будет следить за Secret и перекатывать поды по смене resourceVersion, чтобы и этот случай стал автоматическим.

### Включение и выключение TLS после создания

Не поддерживается (см. правила неизменяемости `spec.tls` в [концепциях](concepts.md#проверки-на-стороне-apiserver)). Apiserver отклоняет изменение напрямую:

```sh
$ kubectl patch etcdcluster my-cluster --type=merge -p '{"spec":{"tls":null}}'
Error from server (Invalid): ... spec.tls cannot be added to or removed from
  an existing cluster; delete and recreate
```

То же относится к переключению mTLS (добавлению или удалению `operatorClientSecretRef`) и к подстановке другого Secret. Путь — удалить и создать заново.
