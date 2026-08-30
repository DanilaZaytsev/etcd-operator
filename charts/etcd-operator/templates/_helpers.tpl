{{/* Имя чарта (переопределяемое). */}}
{{- define "etcd-operator.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Полный префикс имён ресурсов. */}}
{{- define "etcd-operator.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "etcd-operator.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Общие метки. */}}
{{- define "etcd-operator.labels" -}}
helm.sh/chart: {{ include "etcd-operator.chart" . }}
{{ include "etcd-operator.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Метки селектора — устойчивые; их же использует селектор Service метрик. */}}
{{- define "etcd-operator.selectorLabels" -}}
app.kubernetes.io/name: {{ include "etcd-operator.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* Имя ServiceAccount. */}}
{{- define "etcd-operator.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- include "etcd-operator.fullname" . -}}
{{- else -}}
{{- /* Не откатываться молча на SA "default" неймспейса: rbac.yaml привязывает
широкий ClusterRole оператора именно к этому имени, и привязка его к "default"
раздала бы эти права каждой нагрузке, использующей SA по умолчанию. */ -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Полная ссылка на образ оператора. Используется И для образа контейнера
менеджера, И для его переменной окружения OPERATOR_IMAGE — они ОБЯЗАНЫ
совпадать, иначе оператор откажется стартовать (Job снапшота и init-контейнер
install-tools у сида восстановления запускают этот же образ).
*/}}
{{- define "etcd-operator.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
Метки для объектов EtcdCluster и EtcdSnapshotPolicy, которые рендерит этот чарт.

Намеренно НЕ собственный набор меток оператора (app.kubernetes.io/name: etcd,
ключи etcd-operator.cozystack.io/*): те принадлежат объектам, которые создаёт
оператор, и spec.additionalMetadata отказывается их перезаписывать. Эти метки
относятся к самим CR, которыми оператор не владеет.
*/}}
{{- define "etcd-operator.clusterLabels" -}}
helm.sh/chart: {{ include "etcd-operator.chart" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: etcd-operator
{{- end -}}

{{/*
Расписание бэкапов кластера, где минута детерминированно разносится по часу,
когда включён backup.spread.

Кластеры, описанные из одного блока умолчаний, несут одно и то же выражение
cron, поэтому все снимают снапшот в одну и ту же минуту: на родительском
кластере с сотней с лишним дочерних кластеров это сотня одновременных потоков снапшотов
etcd в одно хранилище на каждом тике. Выводится из неймспейса И имени кластера.
Неймспейс здесь не украшение: там, где каждому дочернему кластеру достаётся свой
неймспейс, естественное имя его кластера во всех них одинаково («etcd»), и
хэширование одного имени даёт каждому дочернему кластеру ту же самую минуту — разнос не
делает ничего, то есть остаётся ровно та толпа, ради предотвращения которой он и
написан, за фасадом функции, которая выглядит работающей.

Устойчиво между обновлениями (перерендер не должен сдвигать окно бэкапа
кластера) и различается между кластерами, причём число никто не выбирает.

Пропускается через sha256 перед adler32, а не только через adler32. adler32 —
контрольная сумма, а не хэш: на именах, различающихся одной цифрой (tenant-001,
tenant-002 — так именует родительский кластер, где всё создаётся программно), её
выход движется синхронно, и минуты, на которые она попадает, имеют структуру. На
160 таких кластерах это оставляло 24 из 60 минут полностью незанятыми, сгружая
остальных в оставшиеся. Проход sha256 при рендере не стоит ничего и разносит их
как надо.

Переписывается только простая числовая минута: выражение с шагом или диапазоном
остаётся ровно таким, как написано, потому что его разнос изменил бы то, о чём
просили.
*/}}
{{- define "etcd-operator.backupSchedule" -}}
{{- $c := .cluster -}}
{{- $schedule := $c.backup.schedule -}}
{{- $fields := splitList " " $schedule -}}
{{- if and $c.backup.spread (eq (len $fields) 5) (regexMatch "^[0-9]+$" (first $fields)) -}}
{{- $minute := mod (atoi (adler32sum (sha256sum (printf "%s/%s" .namespace $c.name)))) 60 -}}
{{- join " " (prepend (rest $fields) (printf "%d" $minute)) -}}
{{- else -}}
{{- $schedule -}}
{{- end -}}
{{- end -}}

{{/*
Несут ли отрендеренные кластеры аннотацию helm.sh/resource-policy: keep.

`helm uninstall` означает две разные вещи в зависимости от того, какую половину
чарта несёт релиз, и одна аннотация не может быть верной для обеих.

Релиз, который ставит ОПЕРАТОРА и перечисляет кластеры: его удаление означает
«убрать оператора». Унести вместе с ним etcd каждого дочернего кластера — и PVC их
членов — было бы катастрофой и ничьим намерением. Сохраняем.

Релиз, который несёт ТОЛЬКО кластеры (operator.enabled=false), то есть форма на
кластера: кластер И ЕСТЬ релиз. Его удаление означает «убрать этот
кластер», и keep оставил бы релиз неспособным управлять единственным
объектом, ради которого он существует, — осиротевшим EtcdCluster, до которого не
дотянется ни одно будущее обновление. Отпускаем.

Поэтому умолчание — «сохранять, когда этот релиз ставит и оператора», и его
выбирает `null`. Явные true или false перекрывают решение в любую сторону.

Учтите: та же асимметрия действует и для `helm upgrade`. Там, где кластеры
сохраняются, удаление записи из `.Values.clusters` кластер НЕ удаляет — Helm его
пропускает. Вывод кластера из сохраняющего релиза — это осознанный
`kubectl delete etcdcluster`, и так задумано.
*/}}
{{- define "etcd-operator.keepClusters" -}}
{{- if kindIs "invalid" .Values.clustersKeepOnUninstall -}}
{{- if .Values.operator.enabled }}true{{ end -}}
{{- else if .Values.clustersKeepOnUninstall -}}
true
{{- end -}}
{{- end -}}
