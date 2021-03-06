package photoprism

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jinzhu/gorm"
	"github.com/photoprism/photoprism/internal/config"
	"github.com/photoprism/photoprism/internal/models"
	log "github.com/sirupsen/logrus"
)

const (
	indexResultUpdated = "updated"
	indexResultAdded   = "added"
)

// Indexer defines an indexer with originals path tensorflow and a db.
type Indexer struct {
	conf       *config.Config
	tensorFlow *TensorFlow
	db         *gorm.DB
}

// NewIndexer returns a new indexer.
// TODO: Is it really necessary to return a pointer?
func NewIndexer(conf *config.Config, tensorFlow *TensorFlow) *Indexer {
	instance := &Indexer{
		conf:       conf,
		tensorFlow: tensorFlow,
		db:         conf.Db(),
	}

	return instance
}

func (i *Indexer) originalsPath() string {
	return i.conf.OriginalsPath()
}

func (i *Indexer) thumbnailsPath() string {
	return i.conf.ThumbnailsPath()
}

// getImageTags returns all tags of a given mediafile. This function returns
// an empty list in the case of an error.
func (i *Indexer) getImageTags(jpeg *MediaFile) (results []*models.Tag) {
	start := time.Now()

	var thumbs []string

	if jpeg.AspectRatio() == 1 {
		thumbs = []string{"tile_224"}
	} else {
		thumbs = []string{"tile_224", "left_224", "right_224"}
	}

	var allLabels TensorFlowLabels

	for _, thumb := range thumbs {
		filename, err := jpeg.Thumbnail(i.thumbnailsPath(), thumb)

		if err != nil {
			log.Error(err)
			continue
		}

		labels, err := i.tensorFlow.GetImageTagsFromFile(filename)

		if err != nil {
			log.Error(err)
			continue
		}

		allLabels = append(allLabels, labels...)
	}

	// Sort by probability
	sort.Sort(TensorFlowLabels(allLabels))

	var max float32 = -1

	for _, l := range allLabels {
		if max == -1 {
			max = l.Probability
		}

		if l.Probability > (max / 3) {
			results = i.appendTag(results, l.Label)
		}
	}

	elapsed := time.Since(start)

	log.Infof("finding %+v labels for %s took %s", allLabels, jpeg.Filename(), elapsed)

	return results
}

func (i *Indexer) appendTag(tags []*models.Tag, label string) []*models.Tag {
	if label == "" {
		return tags
	}

	label = strings.ToLower(label)

	for _, tag := range tags {
		if tag.TagLabel == label {
			return tags
		}
	}

	tag := models.NewTag(label).FirstOrCreate(i.db)

	return append(tags, tag)
}

func (i *Indexer) indexMediaFile(mediaFile *MediaFile) string {
	var photo models.Photo
	var file, primaryFile models.File
	var isPrimary = false
	var tags []*models.Tag

	canonicalName := mediaFile.CanonicalNameFromFile()
	fileHash := mediaFile.Hash()
	relativeFileName := mediaFile.RelativeFilename(i.originalsPath())

	photoQuery := i.db.First(&photo, "photo_canonical_name = ?", canonicalName)

	if photoQuery.Error != nil {
		if jpeg, err := mediaFile.Jpeg(); err == nil {
			// Geo Location
			if exifData, err := jpeg.ExifData(); err == nil {
				photo.PhotoLat = exifData.Lat
				photo.PhotoLong = exifData.Long
				photo.PhotoArtist = exifData.Artist
			}

			// Tags (TensorFlow)
			tags = i.getImageTags(jpeg)
		}

		if location, err := mediaFile.Location(); err == nil {
			i.db.FirstOrCreate(location, "id = ?", location.ID)
			photo.Location = location
			photo.Country = models.NewCountry(location.LocCountryCode, location.LocCountry).FirstOrCreate(i.db)

			tags = i.appendTag(tags, location.LocCity)
			tags = i.appendTag(tags, location.LocCounty)
			tags = i.appendTag(tags, location.LocCountry)
			tags = i.appendTag(tags, location.LocCategory)
			tags = i.appendTag(tags, location.LocName)
			tags = i.appendTag(tags, location.LocType)

			if photo.PhotoTitle == "" && location.LocName != "" && location.LocCity != "" { // TODO: User defined title format
				if len(location.LocName) > 40 {
					photo.PhotoTitle = fmt.Sprintf("%s / %s", strings.Title(location.LocName), mediaFile.DateCreated().Format("2006"))
				} else {
					photo.PhotoTitle = fmt.Sprintf("%s / %s / %s", strings.Title(location.LocName), location.LocCity, mediaFile.DateCreated().Format("2006"))
				}
			} else if photo.PhotoTitle == "" && location.LocCity != "" && location.LocCountry != "" {
				photo.PhotoTitle = fmt.Sprintf("%s / %s / %s", location.LocCity, location.LocCountry, mediaFile.DateCreated().Format("2006"))
			} else if photo.PhotoTitle == "" && location.LocCounty != "" && location.LocCountry != "" {
				photo.PhotoTitle = fmt.Sprintf("%s / %s / %s", location.LocCounty, location.LocCountry, mediaFile.DateCreated().Format("2006"))
			}
		} else {
			log.Debugf("location cannot be determined precisely: %s", err)

			var recentPhoto models.Photo

			if result := i.db.Order(gorm.Expr("ABS(DATEDIFF(taken_at, ?)) ASC", mediaFile.DateCreated())).Preload("Country").First(&recentPhoto); result.Error == nil {
				if recentPhoto.Country != nil {
					photo.Country = recentPhoto.Country
					log.Debugf("approximate location: %s", recentPhoto.Country.CountryName)
				}
			}
		}

		photo.Tags = tags

		photo.Camera = models.NewCamera(mediaFile.CameraModel(), mediaFile.CameraMake()).FirstOrCreate(i.db)
		photo.Lens = models.NewLens(mediaFile.LensModel(), mediaFile.LensMake()).FirstOrCreate(i.db)
		photo.PhotoFocalLength = mediaFile.FocalLength()
		photo.PhotoAperture = mediaFile.Aperture()

		photo.TakenAt = mediaFile.DateCreated()
		photo.PhotoCanonicalName = canonicalName
		photo.PhotoFavorite = false

		if photo.PhotoTitle == "" {
			if len(photo.Tags) > 0 { // TODO: User defined title format
				photo.PhotoTitle = fmt.Sprintf("%s / %s", strings.Title(photo.Tags[0].TagLabel), mediaFile.DateCreated().Format("2006"))
			} else if photo.Camera.String() != "" && photo.Camera.String() != "Unknown" {
				photo.PhotoTitle = fmt.Sprintf("%s / %s", photo.Camera, mediaFile.DateCreated().Format("January 2006"))
			} else {
				var daytimeString string
				hour := mediaFile.DateCreated().Hour()

				switch {
				case hour < 8:
					daytimeString = "Early Bird"
				case hour < 12:
					daytimeString = "Morning Mood"
				case hour < 17:
					daytimeString = "Carpe Diem"
				case hour < 20:
					daytimeString = "Sunset"
				default:
					daytimeString = "Late Night"
				}

				photo.PhotoTitle = fmt.Sprintf("%s / %s", daytimeString, mediaFile.DateCreated().Format("January 2006"))
			}
		}

		log.Debugf("title: \"%s\"", photo.PhotoTitle)

		i.db.Create(&photo)
	} else if time.Now().Sub(photo.UpdatedAt).Minutes() > 10 { // If updated more than 10 minutes ago
		if jpeg, err := mediaFile.Jpeg(); err == nil {
			photo.Camera = models.NewCamera(mediaFile.CameraModel(), mediaFile.CameraMake()).FirstOrCreate(i.db)
			photo.Lens = models.NewLens(mediaFile.LensModel(), mediaFile.LensMake()).FirstOrCreate(i.db)
			photo.PhotoFocalLength = mediaFile.FocalLength()
			photo.PhotoAperture = mediaFile.Aperture()

			// Geo Location
			if exifData, err := jpeg.ExifData(); err == nil {
				photo.PhotoLat = exifData.Lat
				photo.PhotoLong = exifData.Long
				photo.PhotoArtist = exifData.Artist
			}
		}

		if photo.LocationID == 0 {
			var recentPhoto models.Photo

			if result := i.db.Order(gorm.Expr("ABS(DATEDIFF(taken_at, ?)) ASC", photo.TakenAt)).Preload("Country").First(&recentPhoto); result.Error == nil {
				if recentPhoto.Country != nil {
					photo.Country = recentPhoto.Country
				}
			}
		}

		i.db.Save(&photo)
	}

	if result := i.db.Where("file_type = 'jpg' AND file_primary = 1 AND photo_id = ?", photo.ID).First(&primaryFile); result.Error != nil {
		isPrimary = mediaFile.Type() == FileTypeJpeg
	} else {
		isPrimary = mediaFile.Type() == FileTypeJpeg && (relativeFileName == primaryFile.FileName || fileHash == primaryFile.FileHash)
	}

	fileQuery := i.db.First(&file, "file_hash = ? OR file_name = ?", fileHash, relativeFileName)

	file.PhotoID = photo.ID
	file.FilePrimary = isPrimary
	file.FileMissing = false
	file.FileName = relativeFileName
	file.FileHash = fileHash
	file.FileType = mediaFile.Type()
	file.FileMime = mediaFile.MimeType()
	file.FileOrientation = mediaFile.Orientation()

	// Color information
	if p, err := mediaFile.Colors(i.thumbnailsPath()); err == nil {
		file.FileMainColor = p.MainColor.Name()
		file.FileColors = p.Colors.Hex()
		file.FileLuminance = p.Luminance.Hex()
		file.FileChroma = p.Chroma.Uint()
	}

	if mediaFile.Width() > 0 && mediaFile.Height() > 0 {
		file.FileWidth = mediaFile.Width()
		file.FileHeight = mediaFile.Height()
		file.FileAspectRatio = mediaFile.AspectRatio()
		file.FilePortrait = mediaFile.Width() < mediaFile.Height()
	}

	if fileQuery.Error == nil {
		i.db.Save(&file)
		return indexResultUpdated
	}

	i.db.Create(&file)
	return indexResultAdded
}

// IndexRelated will index all mediafiles which has relate to a given mediafile.
func (i *Indexer) IndexRelated(mediaFile *MediaFile) map[string]bool {
	indexed := make(map[string]bool)

	relatedFiles, mainFile, err := mediaFile.RelatedFiles()

	if err != nil {
		log.Warnf("could not index \"%s\": %s", mediaFile.RelativeFilename(i.originalsPath()), err.Error())

		return indexed
	}

	mainIndexResult := i.indexMediaFile(mainFile)
	indexed[mainFile.Filename()] = true

	log.Infof("%s main %s file \"%s\"", mainIndexResult, mainFile.Type(), mainFile.RelativeFilename(i.originalsPath()))

	for _, relatedMediaFile := range relatedFiles {
		if indexed[relatedMediaFile.Filename()] {
			continue
		}

		indexResult := i.indexMediaFile(relatedMediaFile)
		indexed[relatedMediaFile.Filename()] = true

		log.Infof("%s related %s file \"%s\"", indexResult, relatedMediaFile.Type(), relatedMediaFile.RelativeFilename(i.originalsPath()))
	}

	return indexed
}

// IndexAll will index all mediafiles.
func (i *Indexer) IndexAll() map[string]bool {
	indexed := make(map[string]bool)

	err := filepath.Walk(i.originalsPath(), func(filename string, fileInfo os.FileInfo, err error) error {
		if err != nil || indexed[filename] {
			return nil
		}

		if fileInfo.IsDir() || strings.HasPrefix(filepath.Base(filename), ".") {
			return nil
		}

		mediaFile, err := NewMediaFile(filename)

		if err != nil || !mediaFile.IsPhoto() {
			return nil
		}

		for relatedFilename := range i.IndexRelated(mediaFile) {
			indexed[relatedFilename] = true
		}

		return nil
	})

	if err != nil {
		log.Warn(err.Error())
	}

	return indexed
}
