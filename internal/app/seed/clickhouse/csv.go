package seed

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/google/uuid"
)

type rees46Event struct {
	eventID          string
	distinctID       string
	kind             string
	occurTime        time.Time
	autoProperties   map[string]string
	customProperties map[string]string
}

type rees46Reader struct {
	file   *os.File
	reader *csv.Reader
}

func newRees46Reader(path string) (*rees46Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}

	r := csv.NewReader(f)
	r.ReuseRecord = true

	if _, err := r.Read(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read header: %w", err)
	}

	return &rees46Reader{file: f, reader: r}, nil
}

func (r *rees46Reader) Close() error {
	return r.file.Close()
}

func (r *rees46Reader) Read() (*rees46Event, error) {
	record, err := r.reader.Read()
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		return nil, fmt.Errorf("read record: %w", err)
	}

	if len(record) < 8 {
		return nil, fmt.Errorf("record has insufficient columns: %d", len(record))
	}

	eventTime, err := time.Parse("2006-01-02 15:04:05", record[0])
	if err != nil {
		eventTime, err = time.Parse("2006-01-02T15:04:05", record[0])
		if err != nil {
			eventTime, err = time.Parse("2006-01-02 15:04:05 UTC", record[0])
			if err != nil {
				return nil, fmt.Errorf("parse event_time: %w", err)
			}
		}
	}

	maybeEventType := record[1]
	kind := inferKind(maybeEventType)

	var orderID, productID, categoryID, categoryCode, brand, price, userID string
	if isEventType(maybeEventType) {
		orderID = ""
		productID = record[2]
		categoryID = record[3]
		categoryCode = record[4]
		brand = record[5]
		price = record[6]
		userID = record[7]
	} else {
		orderID = record[1]
		productID = record[2]
		categoryID = record[3]
		categoryCode = record[4]
		brand = record[5]
		price = record[6]
		userID = record[7]
	}

	customProps := map[string]string{
		"product_id":    productID,
		"category_id":   categoryID,
		"category_code": categoryCode,
		"brand":         brand,
		"price":         price,
	}

	if orderID != "" {
		customProps["order_id"] = orderID
	}

	return &rees46Event{
		eventID:          uuid.New().String(),
		distinctID:       userID,
		kind:             kind,
		occurTime:        eventTime,
		autoProperties:   map[string]string{},
		customProperties: customProps,
	}, nil
}

func isEventType(s string) bool {
	return s == "view" || s == "cart" || s == "purchase"
}

func inferKind(eventTypeOrOrderID string) string {
	if isEventType(eventTypeOrOrderID) {
		switch eventTypeOrOrderID {
		case "view":
			return "page_view"
		case "cart":
			return "add_to_cart"
		case "purchase":
			return "purchase"
		}
	}

	if eventTypeOrOrderID != "" {
		return "purchase"
	}
	return "page_view"
}
