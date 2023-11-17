package logic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"GoMusic/common/utils"
	"GoMusic/initialize/log"

	"GoMusic/common/models"
	"GoMusic/httputil"
	"GoMusic/repo/cache"
)

const (
	netEasyRedis = "net:%v"
	netEasyUrlV6 = "https://music.163.com/api/v6/playlist/detail"
	netEasyUrlV3 = "https://music.163.com/api/v3/song/detail"
	chunkSize    = 500
)

func NetEasyDiscover(link string) (*models.SongList, error) {
	// 获取歌单 songListId
	songListId, err := utils.GetSongsId(link)
	if err != nil {
		return nil, err
	}

	// 第一次发送请求，获取歌曲 Id 列表
	res, err := httputil.Post(netEasyUrlV6, strings.NewReader("id="+songListId))
	if err != nil {
		log.Errorf("fail to result: %v", err)
		return nil, err
	}
	defer res.Body.Close()

	body, _ := io.ReadAll(res.Body)
	SongIdsResp := &models.NetEasySongId{}
	err = json.Unmarshal(body, SongIdsResp)
	if err != nil {
		log.Errorf("fail to unmarshal: %v", err)
		return nil, err
	}
	// 无权限访问
	if SongIdsResp.Code == 401 {
		log.Errorf("无权限访问, link: %v", link)
		return nil, errors.New("抱歉，您无权限访问该歌单")
	}

	trackIds := SongIdsResp.Playlist.TrackIds // 歌曲列表
	songCacheKey := make([]string, 0, len(trackIds))
	for _, v := range trackIds {
		songCacheKey = append(songCacheKey, fmt.Sprintf(netEasyRedis, v.Id))
	}

	// 尝试获取缓存，失败不退出
	cacheResult, _ := cache.MGet(songCacheKey...)

	missKey := make([]*models.SongId, 0)
	resultMap := sync.Map{}
	for k, v := range cacheResult {
		if v != nil {
			resultMap.Store(trackIds[k].Id, v.(string))
			continue
		}
		missKey = append(missKey, &models.SongId{Id: trackIds[k].Id})
	}
	// 全部命中，直接返回
	if len(missKey) == 0 {
		log.Infof("全部命中缓存（网易云）: %v", link)
		return &models.SongList{
			Name:       SongIdsResp.Playlist.Name,
			Songs:      utils.SyncMapToSortedSlice(trackIds, resultMap),
			SongsCount: SongIdsResp.Playlist.TrackCount,
		}, nil
	}

	// TODO 11.17 查数据库

	missKeyCacheMap, err := batchGetSongs(missKey, resultMap)
	if err != nil {
		return nil, err
	}

	// 写缓存
	_ = cache.MSet(missKeyCacheMap)

	return &models.SongList{
		Name:       SongIdsResp.Playlist.Name,
		Songs:      utils.SyncMapToSortedSlice(trackIds, resultMap),
		SongsCount: SongIdsResp.Playlist.TrackCount,
	}, nil
}

// 批量从网易云音乐查询歌曲数据
func batchGetSongs(missKey []*models.SongId, resultMap sync.Map) (sync.Map, error) {
	// errgroup 并发编程
	missSize := len(missKey)
	errgroup := errgroup.Group{}
	chunks := make([][]*models.SongId, 0, missSize/500+1)
	missKeyCacheMap := sync.Map{}

	for i := 0; i < missSize; i += chunkSize {
		end := i + chunkSize
		if end > missSize {
			end = missSize
		}
		chunks = append(chunks, missKey[i:end])
	}
	for _, v := range chunks {
		chunk := v
		errgroup.Go(func() error {
			marshal, _ := json.Marshal(chunk)
			resp, err := httputil.Post(netEasyUrlV3, strings.NewReader("c="+string(marshal)))
			if err != nil {
				log.Errorf("fail to result: %v", err)
				return err
			}
			defer resp.Body.Close()
			bytes, _ := io.ReadAll(resp.Body)
			songs := &models.Songs{}
			err = json.Unmarshal(bytes, &songs)
			if err != nil {
				log.Errorf("fail to unmarshal: %v", err)
				return err
			}

			builder := strings.Builder{}
			for _, v := range songs.Songs {
				builder.Reset()
				// 去除多余符号
				builder.WriteString(utils.StandardSongName(v.Name))
				builder.WriteString(" - ")

				authors := make([]string, 0, len(v.Ar))
				for _, v := range v.Ar {
					authors = append(authors, v.Name)
				}
				authorsString := strings.Join(authors, " / ")
				builder.WriteString(authorsString)
				song := builder.String()
				missKeyCacheMap.Store(fmt.Sprintf(netEasyRedis, v.Id), song)
				resultMap.Store(v.Id, song)
			}
			return nil
		})
	}
	// 等待所有 goroutine 完成
	if err := errgroup.Wait(); err != nil {
		log.Errorf("fail to wait: %v", err)
		return sync.Map{}, err
	}
	return missKeyCacheMap, nil
}
