package image

import (
	"archive/tar"
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/client"
	"github.com/wagoodman/dive/filetree"
	"golang.org/x/net/context"
	"github.com/wagoodman/jotframe"
	"github.com/k0kubun/go-ansi"
)

// TODO: this file should be rethought... but since it's only for preprocessing it'll be tech debt for now.

func check(e error) {
	if e != nil {
		panic(e)
	}
}

type ProgressBar struct {
	percent     int
	rawTotal    int64
	rawCurrent  int64
}

func NewProgressBar(total int64) *ProgressBar{
	return &ProgressBar{
		rawTotal: total,
	}
}

func (pb *ProgressBar) Done() {
	pb.rawCurrent = pb.rawTotal
	pb.percent = 100
}

func (pb *ProgressBar) Update(currentValue int64) (hasChanged bool) {
	pb.rawCurrent = currentValue
	percent := int(100.0*(float64(pb.rawCurrent) / float64(pb.rawTotal)))
	if percent != pb.percent {
		hasChanged = true
	}
	pb.percent = percent
	return hasChanged
}

func (pb *ProgressBar) String() string {
	width := 40
	done := int((pb.percent*width)/100.0)
	todo := width - done
	head := 1
	// if pb.percent >= 100 {
	// 	head = 0
	// }

	return "[" + strings.Repeat("=", done) + strings.Repeat(">", head) + strings.Repeat(" ", todo) + "]" + fmt.Sprintf(" %d %% (%d/%d)", pb.percent, pb.rawCurrent, pb.rawTotal)
}

type ImageManifest struct {
	ConfigPath    string   `json:"Config"`
	RepoTags      []string `json:"RepoTags"`
	LayerTarPaths []string `json:"Layers"`
}

type ImageConfig struct {
	History []ImageHistoryEntry `json:"history"`
	RootFs RootFs `json:"rootfs"`
}

type RootFs struct {
	Type string `json:"type"`
	DiffIds []string `json:"diff_ids"`
}

type ImageHistoryEntry struct {
	ID string
	Size uint64
	Created string `json:"created"`
	Author string `json:"author"`
	CreatedBy string `json:"created_by"`
	EmptyLayer bool `json:"empty_layer"`
}

func NewImageManifest(reader *tar.Reader, header *tar.Header) ImageManifest {
	size := header.Size
	manifestBytes := make([]byte, size)
	_, err := reader.Read(manifestBytes)
	if err != nil && err != io.EOF {
		panic(err)
	}
	var manifest []ImageManifest
	err = json.Unmarshal(manifestBytes, &manifest)
	if err != nil {
		panic(err)
	}
	return manifest[0]
}

func NewImageConfig(reader *tar.Reader, header *tar.Header) ImageConfig {
	size := header.Size
	configBytes := make([]byte, size)
	_, err := reader.Read(configBytes)
	if err != nil && err != io.EOF {
		panic(err)
	}
	var imageConfig ImageConfig
	err = json.Unmarshal(configBytes, &imageConfig)
	if err != nil {
		panic(err)
	}

	layerIdx := 0
	for idx := range imageConfig.History {
		if imageConfig.History[idx].EmptyLayer {
			imageConfig.History[idx].ID = "<missing>"
		} else {
			imageConfig.History[idx].ID = imageConfig.RootFs.DiffIds[layerIdx]
			layerIdx++
		}
	}

	return imageConfig
}

func GetImageConfig(imageTarPath string, manifest ImageManifest) ImageConfig{
	var config ImageConfig
	// read through the image contents and build a tree
	fmt.Println("  Fetching image config...")
	tarFile, err := os.Open(imageTarPath)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer tarFile.Close()

	tarReader := tar.NewReader(tarFile)
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		name := header.Name
		if name == manifest.ConfigPath {
			config = NewImageConfig(tarReader, header)
		}
	}

	// obtain the image history
	return config
}

func processLayerTar(line *jotframe.Line, layerMap map[string]*filetree.FileTree, name string, tarredBytes []byte) {
	tree := filetree.NewFileTree()
	tree.Name = name

	fileInfos := getFileList(tarredBytes)

	shortName := name[:15]
	pb := NewProgressBar(int64(len(fileInfos)))
	for idx, element := range fileInfos {
		tree.FileSize += uint64(element.TarHeader.FileInfo().Size())
		tree.AddPath(element.Path, element)

		if pb.Update(int64(idx)) {
			io.WriteString(line, fmt.Sprintf("    ├─ %s : %s", shortName, pb.String()))
		}
	}
	pb.Done()
	io.WriteString(line, fmt.Sprintf("    ├─ %s : %s", shortName, pb.String()))

	layerMap[tree.Name] = tree
	line.Close()
}

func InitializeData(imageID string) ([]*Layer, []*filetree.FileTree) {
	var manifest ImageManifest
	var layerMap = make(map[string]*filetree.FileTree)
	var trees = make([]*filetree.FileTree, 0)

	ansi.CursorHide()

	// save this image to disk temporarily to get the content info
	imageTarPath, tmpDir := saveImage(imageID)
	// imageTarPath := "/tmp/dive516670682/image.tar"
	// tmpDir := "/tmp/dive516670682"
	// fmt.Println(tmpDir)
	defer os.RemoveAll(tmpDir)

	// read through the image contents and build a tree
	tarFile, err := os.Open(imageTarPath)
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	defer tarFile.Close()

	fi, err := tarFile.Stat()
	if err != nil {
		panic(err)
	}
	totalSize := fi.Size()
	var observedBytes int64
	var percent int

	tarReader := tar.NewReader(tarFile)
	frame := jotframe.NewFixedFrame(1, true, false, false)
	lastLine := frame.Lines()[0]
	io.WriteString(lastLine, "    ╧")
	lastLine.Close()

	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			io.WriteString(frame.Header(), "  Discovering layers... Done!")
			break
		}

		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		observedBytes += header.Size
		percent = int(100.0*(float64(observedBytes) / float64(totalSize)))
		io.WriteString(frame.Header(), fmt.Sprintf("  Discovering layers... %d %%", percent))

		name := header.Name

		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			if strings.HasSuffix(name, "layer.tar") {
				line, err := frame.Prepend()
				if err != nil {
					panic(err)
				}
				shortName := name[:15]
				io.WriteString(line, "    ├─ " + shortName + " : loading...")

				var tarredBytes = make([]byte, header.Size)

				_, err = tarReader.Read(tarredBytes)
				if err != nil && err != io.EOF {
					panic(err)
				}


				go processLayerTar(line, layerMap, name, tarredBytes)

			} else if name == "manifest.json" {
				manifest = NewImageManifest(tarReader, header)
			}
		default:
			fmt.Printf("ERRG: unknown tar entry: %v: %s\n", header.Typeflag, name)
		}
	}
	frame.Header().Close()
	frame.Wait()
	frame.Remove(lastLine)
	fmt.Println("")

	// obtain the image history
	config := GetImageConfig(imageTarPath, manifest)

	// build the content tree
	fmt.Println("  Building tree...")
	for _, treeName := range manifest.LayerTarPaths {
		trees = append(trees, layerMap[treeName])
	}

	// build the layers array
	layers := make([]*Layer, len(trees))

	// note that the image config stores images in reverse chronological order, so iterate backwards through layers
	// as you iterate chronologically through history (ignoring history items that have no layer contents)
	layerIdx := len(trees)-1
	for idx := 0; idx < len(config.History); idx++ {
		// ignore empty layers, we are only observing layers with content
		if config.History[idx].EmptyLayer {
			continue
		}

		config.History[idx].Size = uint64(trees[(len(trees)-1)-layerIdx].FileSize)

		layers[layerIdx] = &Layer{
			History: config.History[idx],
			Index: layerIdx,
			Tree: trees[layerIdx],
			RefTrees: trees,
		}

		if len(manifest.LayerTarPaths) > idx {
			layers[layerIdx].TarPath = manifest.LayerTarPaths[layerIdx]
		}
		layerIdx--
	}

	ansi.CursorShow()

	return layers, trees
}

func saveImage(imageID string) (string, string) {
	ctx := context.Background()
	dockerClient, err := client.NewClientWithOpts()
	if err != nil {
		panic(err)
	}

	frame := jotframe.NewFixedFrame(0, false, false, true)
	line, err := frame.Append()
	check(err)
	io.WriteString(line, "  Fetching metadata...")

	result, _, err := dockerClient.ImageInspectWithRaw(ctx, imageID)
	check(err)
	totalSize := result.Size

	frame.Remove(line)
	line, err = frame.Append()
	check(err)
	io.WriteString(line, "  Fetching image...")

	readCloser, err := dockerClient.ImageSave(ctx, []string{imageID})
	check(err)
	defer readCloser.Close()

	tmpDir, err := ioutil.TempDir("", "dive")
	check(err)

	imageTarPath := filepath.Join(tmpDir, "image.tar")
	imageFile, err := os.Create(imageTarPath)
	check(err)

	defer func() {
		if err := imageFile.Close(); err != nil {
			panic(err)
		}
	}()
	imageWriter := bufio.NewWriter(imageFile)
	pb := NewProgressBar(totalSize)

	var observedBytes int64

	buf := make([]byte, 1024)
	for {
		n, err := readCloser.Read(buf)
		if err != nil && err != io.EOF {
			panic(err)
		}
		if n == 0 {
			break
		}

		observedBytes += int64(n)

		if pb.Update(observedBytes) {
			io.WriteString(line, fmt.Sprintf("  Fetching image... %s", pb.String()))
		}

		if _, err := imageWriter.Write(buf[:n]); err != nil {
			panic(err)
		}
	}

	if err = imageWriter.Flush(); err != nil {
		panic(err)
	}

	pb.Done()
	io.WriteString(line, fmt.Sprintf("  Fetching image... %s", pb.String()))
	frame.Close()

	return imageTarPath, tmpDir
}

func getFileList(tarredBytes []byte) []filetree.FileInfo {
	var files []filetree.FileInfo

	reader := bytes.NewReader(tarredBytes)
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		name := header.Name

		switch header.Typeflag {
		case tar.TypeXGlobalHeader:
			fmt.Printf("ERRG: XGlobalHeader: %v: %s\n", header.Typeflag, name)
		case tar.TypeXHeader:
			fmt.Printf("ERRG: XHeader: %v: %s\n", header.Typeflag, name)
		default:
			files = append(files, filetree.NewFileInfo(tarReader, header, name))
		}
	}
	return files
}