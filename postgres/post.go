package postgres

import (
	"fmt"
	"github.com/ConstaConst/technopark-db-forum-api/models"
	"github.com/ConstaConst/technopark-db-forum-api/restapi/operations"
	"github.com/go-openapi/runtime/middleware"
	"github.com/go-openapi/strfmt"
	"github.com/jackc/pgx"
	"github.com/jackc/pgx/pgtype"
	"log"
)

func (conn *DBConn) CreatePosts(
	params operations.PostsCreateParams) middleware.Responder {
	tx, _ := conn.pool.Begin()
	defer tx.Rollback()

	log.Printf("Create Posts. Params: in thread=%s, posts count=%d\n",
		params.SlugOrID, len(params.Posts))

	if len(params.Posts) == 0 {
		log.Println("Nothing to insert")

		return operations.NewPostsCreateCreated().WithPayload(models.Posts{})
	}

	thread, err := getThread(tx, params.SlugOrID)
	if err != nil {
		tx.Rollback()

		log.Println("Can't find thread by slug or id=", params.SlugOrID)

		notFoundThreadError := models.Error{Message: fmt.Sprintf(
			"Can't find thread by slug or id=%s", params.SlugOrID)}

		return operations.NewPostsCreateNotFound().WithPayload(
			&notFoundThreadError)
	}

	var args []interface{}
	query := "INSERT INTO posts (author, message, forum, thread, parent) " +
		"VALUES "
	j := 1
	for i, post := range params.Posts {
		if post.Parent != 0 {

			_, err = getPost(tx, post.Parent)
			if err != nil {
				tx.Rollback()

				log.Println("Can't find post parent:", post.Parent)

				notFoundParentError := models.Error{Message: fmt.Sprintf(
					"Can't find post parent=%d", post.Parent)}
				return operations.NewPostsCreateConflict().WithPayload(
					&notFoundParentError)
			}
		}

		query += fmt.Sprintf("($%d,$%d,$%d,$%d,$%d) ",
			j, j+1, j+2, j+3, j+4)
		if i < len(params.Posts)-1 {
			query += ", "
		}
		j += 5
		args = append(args, post.Author, post.Message, thread.Forum, thread.ID, post.Parent)
	}
	query += "RETURNING *;"

	rows, err := tx.Query(query, args...)
	checkError(err)

	posts := models.Posts{}
	for rows.Next() {
		post := models.Post{}
		fetchedCreated := pgtype.Timestamptz{}
		var _tPath interface{}
		err = rows.Scan(&post.ID, &post.Author, &post.Message, &post.Forum,
			&post.Thread, &post.Parent, &fetchedCreated, &post.IsEdited, _tPath)
		checkError(err)
		t := strfmt.NewDateTime()
		err = t.Scan(fetchedCreated.Time)
		checkError(err)
		post.Created = &t

		posts = append(posts, &post)
	}

	for _, post := range posts {
		_, err = tx.Exec(`UPDATE posts 
                     SET path=(SELECT path FROM posts WHERE id=$1)||$2 
                     WHERE id=$2`, post.Parent, post.ID)
		checkError(err)
	}

	_, err = tx.Exec(
		"UPDATE forums SET postsNumber=postsNumber+$1 WHERE slug=$2",
		len(posts), thread.Forum)
	checkError(err)
	_, err = tx.Exec(
		"UPDATE service SET postsNumber=postsNumber+$1", len(posts))
	checkError(err)

	tx.Commit()

	log.Println("Posts are created:", len(posts))

	return operations.NewPostsCreateCreated().WithPayload(posts)
}

func (conn *DBConn) GetPosts(
	params operations.ThreadGetPostsParams) middleware.Responder {
	tx, _ := conn.pool.Begin()
	defer tx.Rollback()

	log.Printf("Get posts in thread=%s. Params: desc=%d, limit=%d, since=%d,"+
		" sort=%s.",
		params.SlugOrID, params.Desc, params.Limit,
		params.Since, *params.Sort)

	thread, err := getThread(tx, params.SlugOrID)
	if err != nil {
		tx.Rollback()

		log.Println("Can't find thread by slug or id=", params.SlugOrID)

		notFoundThreadError := models.Error{Message: fmt.Sprintf(
			"Can't find thread by slug or id=%s", params.SlugOrID)}

		return operations.NewThreadGetPostsNotFound().WithPayload(
			&notFoundThreadError)
	}

	var args []interface{}
	var query string
	if params.Sort != nil {
		switch *params.Sort {
		case "flat":
			query, args = makeQueryFlat(&params, thread.ID)
		case "tree":
			query, args = makeQueryTree(&params, thread.ID)
		case "parent_tree":
			query, args = makeQueryParentTree(tx, &params, thread.ID)
		default:
			return middleware.NotImplemented("Unkwnown sort type")
		}
	}
	if query == "" {
		log.Println("Not found available posts")

		return operations.NewThreadGetPostsOK().WithPayload(models.Posts{})
	}
	log.Println("Query: ", query)
	log.Println("Args:", args)
	rows, err := tx.Query(query, args...)
	checkError(err)

	posts := models.Posts{}
	for rows.Next() {
		post := models.Post{}
		fetchedCreated := pgtype.Timestamptz{}
		err = rows.Scan(&post.Author, &fetchedCreated, &post.Forum, &post.ID,
			&post.Message, &post.Thread, &post.Parent)
		checkError(err)
		t := strfmt.NewDateTime()
		err = t.Scan(fetchedCreated.Time)
		checkError(err)
		post.Created = &t

		posts = append(posts, &post)
	}
	tx.Commit()

	log.Println("Fetched posts ", len(posts))

	return operations.NewThreadGetPostsOK().WithPayload(posts)
}

func makeQueryFlat(params *operations.ThreadGetPostsParams, thread int32) (string, []interface{}) {
	log.Println("Make flat query")

	var args []interface{}
	query := "SELECT author, created, forum, id, message, thread, parent " +
		"FROM posts " +
		"WHERE thread=$1 "
	args = append(args, thread)

	if params.Since != nil {
		args = append(args, *params.Since)
		if params.Desc != nil && *params.Desc {
			query += fmt.Sprintf("AND id < $%d ", len(args))
		} else {
			query += fmt.Sprintf("AND id > $%d ", len(args))
		}
	}
	query += "ORDER BY id "
	if params.Desc != nil && *params.Desc {
		query += "DESC "
	}

	if params.Limit != nil {
		args = append(args, *params.Limit)
		query += fmt.Sprintf("LIMIT $%d", len(args))
	}

	return query, args
}

func makeQueryTree(params *operations.ThreadGetPostsParams, thread int32) (string, []interface{}) {
	log.Println("Make tree query")

	var args []interface{}
	query := "SELECT author, created, forum, id, message, thread, parent " +
		"FROM posts " +
		"WHERE thread=$1 "
	args = append(args, thread)

	if params.Since != nil {
		args = append(args, *params.Since)
		if params.Desc != nil && *params.Desc {
			query += fmt.Sprintf(
				"AND path < (select path from posts where id=$%d)", len(args))
		} else {
			query += fmt.Sprintf(
				"AND path > (select path from posts where id=$%d) ", len(args))
		}
	}
	query += "ORDER BY path "
	if params.Desc != nil && *params.Desc {
		query += "DESC "
	}

	if params.Limit != nil {
		args = append(args, *params.Limit)
		query += fmt.Sprintf("LIMIT $%d", len(args))
	}

	return query, args
}

func makeQueryParentTree(tx *pgx.Tx, params *operations.ThreadGetPostsParams, thread int32) (string, []interface{}) {
	log.Println("Make parent tree query")

	var args []interface{}
	query := "SELECT author, created, forum, id, message, thread, parent " +
		"FROM posts " +
		"WHERE thread=$1 "
	args = append(args, thread)

	if params.Since != nil || params.Desc != nil || params.Limit != nil {
		roots, _ := getRootPosts(tx, params, thread)
		if len(roots) == 0 {
			return "", args
		}
		rootsStr := ""
		for i, root := range roots {
			args = append(args, root)
			rootsStr += fmt.Sprintf("path && ARRAY[$%d]::bigint[] ", len(args))

			if i < len(roots)-1 {
				rootsStr += "OR "
			}
		}
		query += "AND (" + rootsStr + ")"
	}

	if params.Desc != nil && *params.Desc {
		query += "ORDER BY path[1] DESC, path "
	} else {
		query += "ORDER BY path "
	}

	return query, args
}

func getRootPosts(tx *pgx.Tx, params *operations.ThreadGetPostsParams, thread int32) ([]int64, error) {
	subQuery := "SELECT id " +
		"FROM posts " +
		"WHERE thread=$1 AND array_length(path, 1)=1 "
	var subArgs []interface{}
	subArgs = append(subArgs, thread)

	if params.Since != nil {
		subArgs = append(subArgs, *params.Since)
		if params.Desc != nil && *params.Desc {
			subQuery += fmt.Sprintf("AND path < ARRAY[(select path[1] from posts where id=$%d)] ", len(subArgs))
		} else {
			subQuery += fmt.Sprintf("AND path > ARRAY[(select path from posts where id=$%d)] ", len(subArgs))
		}
	}

	subQuery += "ORDER BY path "
	if params.Desc != nil && *params.Desc {
		subQuery += "DESC "
	}

	if params.Limit != nil {
		subArgs = append(subArgs, *params.Limit)
		subQuery += fmt.Sprintf("LIMIT $%d ", len(subArgs))
	}

	log.Println("subQuery:", subQuery)
	log.Println("subArgs:", subArgs)

	rows, err := tx.Query(subQuery, subArgs...)
	checkError(err)

	var roots []int64

	for rows.Next() {
		var root int64
		err = rows.Scan(&root)
		checkError(err)

		roots = append(roots, root)
	}

	return roots, nil
}
