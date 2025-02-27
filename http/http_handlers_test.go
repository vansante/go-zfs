package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	zfs "github.com/vansante/go-zfsutils"

	"github.com/stretchr/testify/require"
)

const (
	testZPool          = "go-test-zpool-http"
	testFilesystemName = "filesys1"
	testPrefix         = "/test-prefix"
	testFilesystem     = testZPool + "/" + testFilesystemName
)

func httpHandlerTest(t *testing.T, fn func(url string)) {
	t.Helper()
	TestHTTPZPool(testZPool, testPrefix, testFilesystem, func(server *httptest.Server) {
		fn(server.URL + testPrefix)
	})
}

func TestHTTP_handleListFilesystems(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/filesystems", url), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		var list []zfs.Dataset
		err = json.NewDecoder(resp.Body).Decode(&list)
		require.NoError(t, err)
		require.Len(t, list, 2)
		require.Equal(t, testZPool, list[0].Name)
		require.Equal(t, testFilesystem, list[1].Name)
	})
}

func TestHTTP_handleSetFilesystemProps(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		props := SetProperties{
			Set: map[string]string{"nl.test:test": "helloworld", "nl.test:blaat": "disk"},
		}
		data, err := json.Marshal(&props)
		require.NoError(t, err)

		req, err := http.NewRequest(http.MethodPatch, fmt.Sprintf("%s/filesystems/%s?%s=%s",
			url, testFilesystemName,
			GETParamExtraProperties, "nl.test:blaat,nl.test:test",
		), bytes.NewBuffer(data))
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		var ds zfs.Dataset
		err = json.NewDecoder(resp.Body).Decode(&ds)
		require.NoError(t, err)
		require.Equal(t, testFilesystem, ds.Name)
		require.Len(t, ds.ExtraProps, 2)
		require.Equal(t, map[string]string{
			"nl.test:test":  "helloworld",
			"nl.test:blaat": "disk",
		}, ds.ExtraProps)
	})
}

func TestHTTP_handleMakeSnapshot(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName = "snappie"

		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/filesystems/%s/snapshots/%s",
			url, testFilesystemName,
			snapName,
		), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusCreated, resp.StatusCode)

		var ds zfs.Dataset
		err = json.NewDecoder(resp.Body).Decode(&ds)
		require.NoError(t, err)
		name := fmt.Sprintf("%s/%s@%s", testZPool, testFilesystemName, snapName)
		require.Equal(t, name, ds.Name)

		snaps, err := zfs.ListSnapshots(context.Background(), zfs.ListOptions{
			ParentDataset: fmt.Sprintf("%s/%s", testZPool, testFilesystemName),
		})
		require.NoError(t, err)
		require.Len(t, snaps, 1)
		require.Equal(t, name, snaps[0].Name)
	})
}

func TestHTTP_handleGetSnapshot(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName = "snappie"

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)
		_, err = ds.Snapshot(context.Background(), snapName, zfs.SnapshotOptions{})
		require.NoError(t, err)

		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/filesystems/%s/snapshots/%s",
			url, testFilesystemName,
			snapName,
		), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		testName := fmt.Sprintf("%s/%s", testZPool, "receive")
		ds, err = zfs.ReceiveSnapshot(context.Background(), resp.Body, testName, zfs.ReceiveOptions{
			Resumable:  false,
			Properties: map[string]string{zfs.PropertyCanMount: zfs.ValueOff},
		})
		require.NoError(t, err)
		require.Equal(t, testName, ds.Name)

		snaps, err := zfs.ListSnapshots(context.Background(), zfs.ListOptions{ParentDataset: testName})
		require.NoError(t, err)
		require.Len(t, snaps, 1)
		require.Equal(t, fmt.Sprintf("%s/%s@%s", testZPool, "receive", snapName), snaps[0].Name)
	})
}

func TestHTTP_handleGetSnapshotIncremental(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName1 = "snappie1"
		const snapName2 = "snappie2"

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)
		snap1, err := ds.Snapshot(context.Background(), snapName1, zfs.SnapshotOptions{})
		require.NoError(t, err)
		_, err = ds.Snapshot(context.Background(), snapName2, zfs.SnapshotOptions{})
		require.NoError(t, err)

		// setup the first snapshot without http
		const newFilesys = testZPool + "/inctest"
		pipeRdr, pipeWrtr := io.Pipe()
		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()

			_, err = zfs.ReceiveSnapshot(context.Background(), pipeRdr, newFilesys, zfs.ReceiveOptions{
				Resumable:  false,
				Properties: map[string]string{zfs.PropertyCanMount: zfs.ValueOff},
			})
			require.NoError(t, err)
		}()
		err = snap1.SendSnapshot(context.Background(), pipeWrtr, zfs.SendOptions{Raw: true})
		require.NoError(t, err)
		require.NoError(t, pipeWrtr.Close())
		wg.Wait()

		// Begin the actual test here.
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/filesystems/%s/snapshots/%s/incremental/%s",
			url, testFilesystemName,
			snapName2, snapName1,
		), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		ds, err = zfs.ReceiveSnapshot(context.Background(), resp.Body, newFilesys, zfs.ReceiveOptions{
			Resumable:  false,
			Properties: map[string]string{zfs.PropertyCanMount: zfs.ValueOff},
		})
		require.NoError(t, err)
		require.Equal(t, newFilesys, ds.Name)

		snaps, err := zfs.ListSnapshots(context.Background(), zfs.ListOptions{ParentDataset: newFilesys})
		require.NoError(t, err)
		require.Len(t, snaps, 2)
		require.Equal(t, fmt.Sprintf("%s@%s", newFilesys, snapName1), snaps[0].Name)
		require.Equal(t, fmt.Sprintf("%s@%s", newFilesys, snapName2), snaps[1].Name)
	})
}

func TestHTTP_handleResumeGetSnapshot(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName = "snappie"

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)
		_, err = ds.Snapshot(context.Background(), snapName, zfs.SnapshotOptions{})
		require.NoError(t, err)

		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/filesystems/%s/snapshots/%s",
			url, testFilesystemName,
			snapName,
		), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		testName := fmt.Sprintf("%s/%s", testZPool, "receive")
		ds, err = zfs.ReceiveSnapshot(context.Background(), io.LimitReader(resp.Body, 29_636), testName, zfs.ReceiveOptions{
			Resumable:  true,
			Properties: map[string]string{zfs.PropertyCanMount: zfs.ValueOff},
		})
		require.Error(t, err)
		var recvErr *zfs.ResumableStreamError
		require.True(t, errors.As(err, &recvErr))

		fs, err := zfs.ListFilesystems(context.Background(), zfs.ListOptions{
			ParentDataset:   testName,
			ExtraProperties: []string{zfs.PropertyReceiveResumeToken},
		})
		require.NoError(t, err)
		require.Len(t, fs, 1)
		require.Equal(t, testName, fs[0].Name)
		require.True(t, len(fs[0].ExtraProps[zfs.PropertyReceiveResumeToken]) > 32)
		require.Equal(t, recvErr.ResumeToken(), fs[0].ExtraProps[zfs.PropertyReceiveResumeToken])

		// Now do a resumption on this stream
		req, err = http.NewRequest(http.MethodGet, fmt.Sprintf("%s/snapshot/resume/%s",
			url, fs[0].ExtraProps[zfs.PropertyReceiveResumeToken],
		), nil)
		require.NoError(t, err)

		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.EqualValues(t, http.StatusOK, resp.StatusCode)

		ds, err = zfs.ReceiveSnapshot(context.Background(), resp.Body, testName, zfs.ReceiveOptions{
			Resumable:  true,
			Properties: map[string]string{zfs.PropertyCanMount: zfs.ValueOff},
		})
		require.NoError(t, err)

		snaps, err := zfs.ListSnapshots(context.Background(), zfs.ListOptions{ParentDataset: testName})
		require.NoError(t, err)
		require.Len(t, snaps, 1)
		require.Equal(t, fmt.Sprintf("%s/%s@%s", testZPool, "receive", snapName), snaps[0].Name)
	})
}

func TestHTTP_handleReceiveSnapshot(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName = "send"
		const testProp = "nl.test:dsk"
		const testPropVal = "1234"

		pipeRdr, pipeWrtr := io.Pipe()

		const newFilesystem = "bla"
		const newSnap = "recv"
		req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/filesystems/%s/snapshots/%s?%s=%s",
			url, newFilesystem,
			newSnap,
			GETParamReceiveProperties, ReceiveProperties{
				zfs.PropertyCanMount: zfs.ValueOff,
			}.Encode(),
		), pipeRdr)
		require.NoError(t, err)

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.EqualValues(t, http.StatusCreated, resp.StatusCode)

			ds := zfs.Dataset{}
			err = json.NewDecoder(resp.Body).Decode(&ds)
			require.NoError(t, err)
			name := fmt.Sprintf("%s/%s@%s", testZPool, newFilesystem, newSnap)
			require.Equal(t, name, ds.Name)

			newFs, err := zfs.GetDataset(context.Background(), fmt.Sprintf("%s/%s", testZPool, newFilesystem), testProp)
			require.NoError(t, err)
			require.Equal(t, testPropVal, newFs.ExtraProps[testProp])

			snaps, err := newFs.Snapshots(context.Background(), zfs.ListOptions{})
			require.NoError(t, err)
			require.Len(t, snaps, 1)
			require.Equal(t, name, snaps[0].Name)
		}()

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)

		err = ds.SetProperty(context.Background(), testProp, testPropVal)
		require.NoError(t, err)

		ds, err = ds.Snapshot(context.Background(), snapName, zfs.SnapshotOptions{})
		require.NoError(t, err)
		err = ds.SendSnapshot(context.Background(), pipeWrtr, zfs.SendOptions{Raw: true, IncludeProperties: true})
		require.NoError(t, err)
		require.NoError(t, pipeWrtr.Close())

		wg.Wait()
	})
}

func TestHTTP_handleReceiveSnapshotNoExplicitName(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName = "send"

		pipeRdr, pipeWrtr := io.Pipe()

		const newFilesystem = "bla"
		req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/filesystems/%s/snapshots?%s=%s",
			url, newFilesystem,
			GETParamReceiveProperties, ReceiveProperties{zfs.PropertyCanMount: zfs.ValueOff}.Encode(),
		), pipeRdr)
		require.NoError(t, err)

		wg := sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.EqualValues(t, http.StatusCreated, resp.StatusCode)

			ds := zfs.Dataset{}
			err = json.NewDecoder(resp.Body).Decode(&ds)
			require.NoError(t, err)
			require.Equal(t, fmt.Sprintf("%s/%s", testZPool, newFilesystem), ds.Name)

			snaps, err := zfs.ListSnapshots(context.Background(), zfs.ListOptions{
				ParentDataset: fmt.Sprintf("%s/%s", testZPool, newFilesystem),
			})
			require.NoError(t, err)
			require.Len(t, snaps, 1)
			require.Equal(t, fmt.Sprintf("%s/%s@%s", testZPool, newFilesystem, snapName), snaps[0].Name)
		}()

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)
		ds, err = ds.Snapshot(context.Background(), snapName, zfs.SnapshotOptions{})
		require.NoError(t, err)
		err = ds.SendSnapshot(context.Background(), pipeWrtr, zfs.SendOptions{Raw: true, IncludeProperties: true})
		require.NoError(t, err)
		require.NoError(t, pipeWrtr.Close())

		wg.Wait()
	})
}

func TestHTTP_handleReceiveSnapshotResume(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		const snapName = "send"

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)
		toBeSent, err := ds.Snapshot(context.Background(), snapName, zfs.SnapshotOptions{})
		require.NoError(t, err)

		const newFilesystem = "bla"
		const newSnap = "recv"

		newFullSnap := fmt.Sprintf("%s/%s@%s", testZPool, newFilesystem, newSnap)
		pipeRdr, pipeWrtr := io.Pipe()

		wg := sync.WaitGroup{}
		wg.Add(2)

		go func() {
			defer wg.Done()
			_, err := zfs.ReceiveSnapshot(context.Background(), io.LimitReader(pipeRdr, 28_725), newFullSnap, zfs.ReceiveOptions{
				Resumable:  true,
				Properties: map[string]string{zfs.PropertyCanMount: zfs.ValueOff},
			},
			)
			require.Error(t, err)
		}()

		go func() {
			defer wg.Done()

			time.Sleep(time.Second / 5)
			require.NoError(t, pipeWrtr.Close())
		}()

		err = toBeSent.SendSnapshot(context.Background(), pipeWrtr, zfs.SendOptions{
			Raw:               true,
			IncludeProperties: true,
		})
		require.Error(t, err)
		wg.Wait()

		ds, err = zfs.GetDataset(context.Background(), fmt.Sprintf("%s/%s", testZPool, newFilesystem), zfs.PropertyReceiveResumeToken)
		require.NoError(t, err)
		require.NotEmpty(t, ds.ExtraProps[zfs.PropertyReceiveResumeToken])
		require.True(t, len(ds.ExtraProps[zfs.PropertyReceiveResumeToken]) > 100)

		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/filesystems/%s/resume-token",
			url, newFilesystem,
		), nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
		token := resp.Header.Get(HeaderResumeReceiveToken)
		require.True(t, len(token) > 100)

		t.Logf("Got resume token: %s", ds.ExtraProps[zfs.PropertyReceiveResumeToken])

		pipeRdr, pipeWrtr = io.Pipe()

		// Now do a resume HTTP receive request
		req, err = http.NewRequest(http.MethodPut, fmt.Sprintf("%s/filesystems/%s/snapshots/%s?%s=%s&%s=%s",
			url, newFilesystem,
			newSnap,
			GETParamResumable, "true",
			GETParamReceiveProperties, ReceiveProperties{zfs.PropertyCanMount: zfs.ValueOff}.Encode(),
		), pipeRdr)
		require.NoError(t, err)

		req.Header.Set(HeaderResumeReceiveToken, token)

		wg = sync.WaitGroup{}
		wg.Add(1)
		go func() {
			defer wg.Done()

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.EqualValues(t, http.StatusCreated, resp.StatusCode)

			ds := zfs.Dataset{}
			err = json.NewDecoder(resp.Body).Decode(&ds)
			require.NoError(t, err)
			name := fmt.Sprintf("%s/%s@%s", testZPool, newFilesystem, newSnap)
			require.Equal(t, name, ds.Name)

			snaps, err := zfs.ListSnapshots(context.Background(), zfs.ListOptions{
				ParentDataset: fmt.Sprintf("%s/%s", testZPool, newFilesystem),
			})
			require.NoError(t, err)
			require.Len(t, snaps, 1)
			require.Equal(t, name, snaps[0].Name)
		}()

		err = zfs.ResumeSend(context.Background(), pipeWrtr, token, zfs.ResumeSendOptions{})
		require.NoError(t, err)
		require.NoError(t, pipeWrtr.Close())

		wg.Wait()

		ds, err = zfs.GetDataset(context.Background(), newFullSnap)
		require.NoError(t, err)
		require.Equal(t, ds.Name, newFullSnap)
	})
}

func TestHTTP_handleReceiveSnapshotMaxConcurrent(t *testing.T) {
	httpHandlerTest(t, func(url string) {
		startMutex := sync.RWMutex{}
		startMutex.Lock()
		endWg := sync.WaitGroup{}

		const snapName = "snappy"

		ds, err := zfs.GetDataset(context.Background(), testFilesystem)
		require.NoError(t, err)

		ds, err = ds.Snapshot(context.Background(), snapName, zfs.SnapshotOptions{})
		require.NoError(t, err)

		const newSnap = "recv"
		var countError, countTooMany int32
		endWg.Add(4)
		for i, name := range []string{"bla1", "bla2", "bla3", "bla4"} {
			go func(i int, name string) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond*500)
				defer cancel()

				body := bytes.NewBuffer([]byte{0, 0, 7})
				req, err := http.NewRequestWithContext(ctx, http.MethodPut, fmt.Sprintf("%s/filesystems/%s/snapshots/%s?%s=%s",
					url, name,
					newSnap,
					GETParamReceiveProperties, ReceiveProperties{
						zfs.PropertyCanMount: zfs.ValueOff,
					}.Encode(),
				), body)

				startMutex.RLock()
				defer startMutex.RUnlock()

				resp, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				_ = resp.Body.Close()

				switch resp.StatusCode {
				case http.StatusInternalServerError:
					atomic.AddInt32(&countError, 1)
					endWg.Done()
					t.Logf("%d: Received OK", i)
				case http.StatusTooManyRequests:
					atomic.AddInt32(&countTooMany, 1)
					endWg.Done()
					t.Logf("%d: Received too many requests", i)
				default:
					endWg.Done()
					t.Logf("%d: Got unexpected status code %d", i, resp.StatusCode)
					t.Fail()
				}
			}(i, name)
		}
		startMutex.Unlock()
		time.Sleep(10 * time.Millisecond)
		endWg.Wait()

		require.GreaterOrEqual(t, countTooMany, int32(1), "CountTooMany not returned")
		require.GreaterOrEqual(t, countError, int32(2), "CountError is not at least 2")
	})
}
