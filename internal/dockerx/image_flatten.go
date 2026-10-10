package dockerx

// Legacy image migration exports only an unstarted, unmounted container made
// from an immutable image. The resulting single-layer image keeps the complete
// image config, rather than the lossy subset supported by docker import -c.
import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"agentbox/internal/imageupdate"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
)

const flatSourceLabel = "agentbox.cli-base.source"
const flatSchemaLabel = "agentbox.cli-base.schema"

// CheckCLIImageSpace measures the daemon's filesystem, including remote Docker
// and Docker Desktop. No host path or Docker socket is mounted into the helper.
// The reserve also covers ordinary workspace writes; it is not a disk quota.
func (m *Manager) CheckCLIImageSpace(ctx context.Context, id string, extra int64) error {
	if extra < 0 {
		return errors.New("镜像空间估算无效")
	}
	reserve := int64(1 << 30)
	if m.cfg.GetResources().MinFreeBytes > reserve {
		reserve = m.cfg.GetResources().MinFreeBytes
	}
	if extra > math.MaxInt64-reserve {
		return errors.New("镜像空间估算溢出")
	}
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	output, code, failure := m.runOfflineNode(probe, id, "agentbox.cli-space", `const s=require('node:fs').statfsSync('/'); console.log(BigInt(s.bavail)*BigInt(s.bsize)+'');`, "", 128)
	if failure != "" || code != 0 {
		return errors.New("无法检查 Docker 数据盘可用空间，保留当前镜像")
	}
	free, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil {
		return errors.New("Docker 数据盘空间检查结果无效")
	}
	if free < extra+reserve {
		return fmt.Errorf("Docker 数据盘空间不足：可用 %.1f GiB，本次维护需至少 %.1f GiB（含预留），保留当前镜像", float64(free)/(1<<30), float64(extra+reserve)/(1<<30))
	}
	return nil
}

func (m *Manager) rawCLIImage(ctx context.Context, id string) (image.InspectResponse, json.RawMessage, error) {
	var raw bytes.Buffer
	info, err := m.cli.ImageInspect(ctx, id, client.ImageInspectWithRawResponse(&raw))
	if err != nil {
		return info, nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw.Bytes(), &fields); err != nil {
		return info, nil, err
	}
	if !imageIDRE.MatchString(info.ID) || info.Config == nil || info.Os != "linux" || info.Architecture == "" || len(info.Config.Volumes) > 0 || len(info.Config.OnBuild) > 0 {
		return info, nil, errors.New("镜像维护仅支持无 VOLUME/ONBUILD 的 Linux 镜像，保留当前镜像")
	}
	return info, fields["Config"], nil
}

func flatConfig(raw json.RawMessage, source string) (json.RawMessage, error) {
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("镜像配置为空")
	}
	labels := map[string]string{}
	if v := cfg["Labels"]; len(v) > 0 {
		if err := json.Unmarshal(v, &labels); err != nil {
			return nil, err
		}
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[flatSchemaLabel] = "1"
	labels[flatSourceLabel] = source
	cfg["Labels"], _ = json.Marshal(labels)
	return json.Marshal(cfg)
}

func sameJSON(a, b []byte) bool {
	var x, y any
	da, db := json.NewDecoder(bytes.NewReader(a)), json.NewDecoder(bytes.NewReader(b))
	da.UseNumber()
	db.UseNumber()
	return da.Decode(&x) == nil && db.Decode(&y) == nil && reflect.DeepEqual(x, y)
}

// writeFlatArchive streams a Docker save archive without buffering a filesystem
// in server RAM/disk. Docker may stage the archive on its own disk; the preflight
// reserves three times the measured export plus the configured safety reserve.
func writeFlatArchive(w io.Writer, root io.Reader, size int64, source image.InspectResponse, cfg json.RawMessage, tag string) (string, error) {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Name: "rootfs/layer.tar", Mode: 0600, Size: size}); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, hash), root, size); err != nil {
		return "", err
	}
	var extra [1]byte
	if n, err := root.Read(extra[:]); n != 0 || err != io.EOF {
		return "", errors.New("镜像导出长度变化，未完成导入")
	}
	diff := "sha256:" + hex.EncodeToString(hash.Sum(nil))
	payload := map[string]any{"architecture": source.Architecture, "os": source.Os, "config": cfg, "rootfs": map[string]any{"type": "layers", "diff_ids": []string{diff}}, "history": []any{map[string]any{"created_by": "agentbox fixed CLI base v1"}}}
	if source.Variant != "" {
		payload["variant"] = source.Variant
	}
	if source.Created != "" {
		payload["created"] = source.Created
	}
	if source.Author != "" {
		payload["author"] = source.Author
	}
	if source.OsVersion != "" {
		payload["os.version"] = source.OsVersion
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	id := hex.EncodeToString(sum[:])
	write := func(name string, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(body))}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	if err = write(id+".json", data); err != nil {
		return "", err
	}
	manifest, _ := json.Marshal([]any{map[string]any{"Config": id + ".json", "RepoTags": []string{tag}, "Layers": []string{"rootfs/layer.tar"}}})
	if err = write("manifest.json", manifest); err != nil {
		return "", err
	}
	return "sha256:" + id, tw.Close()
}

func (m *Manager) FlattenCLIImage(ctx context.Context, src imageupdate.Image, log func(string)) (result imageupdate.Image, err error) {
	if !imageIDRE.MatchString(src.ID) {
		return result, errors.New("迁移源镜像 ID 无效")
	}
	info, raw, err := m.rawCLIImage(ctx, src.ID)
	if err != nil {
		return result, err
	}
	cfg, err := flatConfig(raw, src.ID)
	if err != nil {
		return result, err
	}
	name := "agentbox-cli-flatten-" + strings.TrimPrefix(src.ID, "sha256:")
	// Only recover our own never-started export container. Never touch a workspace.
	if old, e := m.cli.ContainerInspect(ctx, name); e == nil {
		if old.Config == nil || old.Config.Labels[flatSourceLabel] != src.ID || old.Image != src.ID || old.State == nil || old.State.Status != "created" || len(old.Mounts) != 0 {
			return result, errors.New("镜像维护临时容器状态异常，未清理")
		}
		if err = m.cli.ContainerRemove(ctx, old.ID, container.RemoveOptions{}); err != nil {
			return result, err
		}
	} else if !client.IsErrNotFound(e) {
		return result, e
	}
	tag := "agentbox-agent:base-" + strings.TrimPrefix(src.ID, "sha256:")
	// Deterministic staging names allow recovery after import or service restart.
	if existing, config, e := m.rawCLIImage(ctx, tag); e == nil {
		if len(existing.RootFS.Layers) != 1 || !sameJSON(cfg, config) || existing.Architecture != info.Architecture || existing.Variant != info.Variant {
			return result, errors.New("镜像维护暂存标签已被其他镜像占用")
		}
		log("复用已生成的固定底座，重新进行行为验证。\n")
		return m.InspectCLIImage(ctx, existing.ID)
	} else if !client.IsErrNotFound(e) {
		return result, e
	}
	// Even measurement must leave headroom for ongoing production writes.
	if err = m.CheckCLIImageSpace(ctx, src.ID, 0); err != nil {
		return result, err
	}
	created, err := m.cli.ContainerCreate(ctx, &container.Config{Image: src.ID, Entrypoint: []string{"/bin/false"}, Cmd: []string{}, NetworkDisabled: true, Labels: map[string]string{flatSourceLabel: src.ID}}, &container.HostConfig{NetworkMode: "none", ReadonlyRootfs: true, CapDrop: []string{"ALL"}, SecurityOpt: []string{"no-new-privileges"}}, nil, nil, name)
	if err != nil {
		return result, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := m.cli.ContainerRemove(cleanup, created.ID, container.RemoveOptions{}); e != nil {
			err = errors.Join(err, fmt.Errorf("清理镜像维护临时容器失败: %w", e))
		}
	}()
	measured, e := m.cli.ContainerExport(ctx, created.ID)
	if e != nil {
		return result, e
	}
	size, e := io.Copy(&imageTransferProgress{phase: "测量", log: log}, measured)
	closeErr := measured.Close()
	if e != nil || closeErr != nil {
		return result, errors.Join(e, closeErr)
	}
	if size <= 0 || size > math.MaxInt64/3 {
		return result, errors.New("镜像导出大小无效")
	}
	log(fmt.Sprintf("镜像最终文件系统约 %.2f GiB；检查新旧镜像共存空间。\n", float64(size)/(1<<30)))
	if err = m.CheckCLIImageSpace(ctx, src.ID, 3*size); err != nil {
		return result, err
	}
	root, e := m.cli.ContainerExport(ctx, created.ID)
	if e != nil {
		return result, e
	}
	defer root.Close()
	reader, writer := io.Pipe()
	type archiveResult struct {
		diff string
		err  error
	}
	done := make(chan archiveResult, 1)
	go func() {
		hash := sha256.New()
		_, e := writeFlatArchive(writer, io.TeeReader(root, io.MultiWriter(hash, &imageTransferProgress{phase: "传输", log: log})), size, info, cfg, tag)
		writer.CloseWithError(e)
		done <- archiveResult{"sha256:" + hex.EncodeToString(hash.Sum(nil)), e}
	}()
	loaded, e := m.cli.ImageLoad(ctx, reader, client.ImageLoadWithQuiet(true))
	if e == nil {
		e = readBuildOutput(loaded.Body, func(string) {})
		e = errors.Join(e, loaded.Body.Close())
	}
	reader.CloseWithError(e)
	root.Close()
	built := <-done
	if e != nil || built.err != nil {
		return result, errors.Join(e, built.err)
	}
	// Classic Docker uses the config digest as ID; containerd image stores use
	// a manifest digest. Resolve the staging tag, then pin the daemon's actual ID
	// and verify the imported rootfs digest AND config, rather than guessing IDs.
	resolved, _, e := m.rawCLIImage(ctx, tag)
	if e != nil {
		return result, e
	}
	after, afterCfg, e := m.rawCLIImage(ctx, resolved.ID)
	if e != nil {
		return result, e
	}
	if len(after.RootFS.Layers) != 1 || after.RootFS.Layers[0] != built.diff || !sameJSON(cfg, afterCfg) || after.Architecture != info.Architecture || after.Variant != info.Variant {
		return result, errors.New("固定底座配置核验失败，未切换镜像")
	}
	log("已生成单层固定底座，原镜像与现有容器保持可用。\n")
	return m.InspectCLIImage(ctx, after.ID)
}

// Keep long migrations observable without buffering file contents or logging
// filenames. The UI intentionally shows indeterminate progress until completion.
type imageTransferProgress struct {
	bytes, next int64
	phase       string
	log         func(string)
}

func (p *imageTransferProgress) Write(b []byte) (int, error) {
	p.bytes += int64(len(b))
	if p.bytes >= p.next+(256<<20) {
		p.next = p.bytes
		p.log(fmt.Sprintf("镜像%s已处理 %.2f GiB。\n", p.phase, float64(p.bytes)/(1<<30)))
	}
	return len(b), nil
}
